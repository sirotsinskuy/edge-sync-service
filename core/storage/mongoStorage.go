package storage

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/open-horizon/edge-sync-service/common"
	"github.com/open-horizon/edge-utilities/logger"
	"github.com/open-horizon/edge-utilities/logger/log"
	"github.com/open-horizon/edge-utilities/logger/trace"
)

type fileHandle struct {
	file    *mgo.GridFile
	session *mgo.Session
	offset  int64
	chunks  map[int64][]byte
}

// MongoStorage is a MongoDB based store
type MongoStorage struct {
	session      *mgo.Session
	dialInfo     *mgo.DialInfo
	openFiles    map[string]*fileHandle
	ticker       *time.Ticker
	connected    bool
	lockChannel  chan int
	mapLock      chan int
	sessionCache []*mgo.Session
	cacheSize    int
	cacheIndex   int
}

type object struct {
	ID                 string                          `bson:"_id"`
	MetaData           common.MetaData                 `bson:"metadata"`
	Status             string                          `bson:"status"`
	RemainingConsumers int                             `bson:"remaining-consumers"`
	RemainingReceivers int                             `bson:"remaining-receivers"`
	Destinations       []common.StoreDestinationStatus `bson:"destinations"`
	LastUpdate         bson.MongoTimestamp             `bson:"last-update"`
}

type destinationObject struct {
	ID          string             `bson:"_id"`
	Destination common.Destination `bson:"destination"`
}

type notificationObject struct {
	ID           string              `bson:"_id"`
	Notification common.Notification `bson:"notification"`
}

type leaderDocument struct {
	ID               int32               `bson:"_id"`
	UUID             string              `bson:"uuid"`
	LastHeartbeatTS  bson.MongoTimestamp `bson:"last-heartbeat-ts"`
	HeartbeatTimeout int32               `bson:"heartbeat-timeout"`
	Version          int64               `bson:"version"`
}

type isMasterResult struct {
	IsMaster  bool      `bson:"isMaster"`
	LocalTime time.Time `bson:"localTime"`
	OK        bool      `bson:"ok"`
}

type messagingGroupObject struct {
	ID         string              `bson:"_id"`
	GroupName  string              `bson:"group-name"`
	LastUpdate bson.MongoTimestamp `bson:"last-update"`
}

// This is almost the same type as common.StoredOrganization except for the timestamp type.
// We use this type here to avoid dependency on bson in common.
type organizationObject struct {
	ID           string              `bson:"_id"`
	Organization common.Organization `bson:"org"`
	LastUpdate   bson.MongoTimestamp `bson:"last-update"`
}

type webhookObject struct {
	ID         string              `bson:"_id"`
	Hooks      []string            `bson:"hooks"`
	LastUpdate bson.MongoTimestamp `bson:"last-update"`
}

type aclObject struct {
	ID         string              `bson:"_id"`
	Usernames  []string            `bson:"usernames"`
	OrgID      string              `bson:"org-id"`
	ACLType    string              `bson:"acl-type"`
	LastUpdate bson.MongoTimestamp `bson:"last-update"`
}

const maxUpdateTries = 5

// Init initializes the MongoStorage store
func (store *MongoStorage) Init() common.SyncServiceError {
	store.lockChannel = make(chan int, 1)
	store.lockChannel <- 1
	store.mapLock = make(chan int, 1)
	store.mapLock <- 1

	store.dialInfo = &mgo.DialInfo{
		Addrs:        strings.Split(common.Configuration.MongoAddressCsv, ","),
		Source:       common.Configuration.MongoAuthDbName,
		Username:     common.Configuration.MongoUsername,
		Password:     common.Configuration.MongoPassword,
		Timeout:      time.Duration(20 * time.Second),
		ReadTimeout:  time.Duration(60 * time.Second),
		WriteTimeout: time.Duration(60 * time.Second),
	}

	if common.Configuration.MongoUseSSL {
		tlsConfig := &tls.Config{}
		if common.Configuration.MongoCACertificate != "" {
			var caFile string
			if strings.HasPrefix(common.Configuration.MongoCACertificate, "/") {
				caFile = common.Configuration.MongoCACertificate
			} else {
				caFile = common.Configuration.PersistenceRootPath + common.Configuration.MongoCACertificate
			}
			serverCaCert, err := ioutil.ReadFile(caFile)
			if err != nil {
				if _, ok := err.(*os.PathError); ok {
					serverCaCert = []byte(common.Configuration.MongoCACertificate)
					err = nil
				} else {
					message := fmt.Sprintf("Failed to find mongo SSL CA file. Error: %s.", err)
					return &Error{message}
				}
			}

			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(serverCaCert)
			tlsConfig.RootCAs = caCertPool
		}

		// Please avoid using this if possible! Makes using TLS pointless
		if common.Configuration.MongoAllowInvalidCertificates {
			tlsConfig.InsecureSkipVerify = true
		}

		store.dialInfo.DialServer = func(addr *mgo.ServerAddr) (net.Conn, error) {
			return tls.Dial("tcp", addr.String(), tlsConfig)
		}
	}

	var session *mgo.Session
	var err error
	for connectTime := 0; connectTime < common.Configuration.DatabaseConnectTimeout; connectTime += 10 {
		session, err = mgo.DialWithInfo(store.dialInfo)
		if err == nil {
			break
		}
		if strings.HasPrefix(err.Error(), "unauthorized") ||
			strings.HasPrefix(err.Error(), "not authorized") ||
			strings.HasPrefix(err.Error(), "auth fail") ||
			strings.HasPrefix(err.Error(), "Authentication failed") {
			break
		}
	}
	if session == nil {
		message := fmt.Sprintf("Failed to dial mgo. Error: %s.", err)
		return &Error{message}
	}

	store.connected = true
	common.HealthStatus.ReconnectedToDatabase()
	if trace.IsLogging(logger.INFO) {
		trace.Info("Connected to the database")
	}
	if log.IsLogging(logger.INFO) {
		log.Info("Connected to the database")
	}

	session.SetSafe(&mgo.Safe{})
	//session.SetMode(mgo.Monotonic, true)

	db := session.DB(common.Configuration.MongoDbName)
	db.C(destinations).EnsureIndexKey("destination.destination-org-id")
	notificationsCollection := db.C(notifications)
	notificationsCollection.EnsureIndexKey("notification.destination-org-id", "notification.destination-id", "notification.destination-type")
	notificationsCollection.EnsureIndexKey("notification.resend-time", "notification.status")
	db.C(objects).EnsureIndexKey("metadata.destination-org-id")
	db.C(acls).EnsureIndexKey("org-id", "acl-type")

	store.session = session
	store.cacheSize = common.Configuration.MongoSessionCacheSize
	if store.cacheSize > 1 {
		store.sessionCache = make([]*mgo.Session, store.cacheSize)
		for i := 0; i < store.cacheSize; i++ {
			store.sessionCache[i] = store.session.Copy()
		}
	}

	store.openFiles = make(map[string]*fileHandle)

	store.ticker = time.NewTicker(time.Second * time.Duration(common.Configuration.StorageMaintenanceInterval))
	go func() {
		for {
			select {
			case <-store.ticker.C:
				store.checkObjects()
			}
		}
	}()

	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Successfully initialized mongo driver")
	}

	return nil
}

// Stop stops the MongoStorage store
func (store *MongoStorage) Stop() {
	if store.cacheSize > 1 {
		for i := 0; i < store.cacheSize; i++ {
			store.sessionCache[i].Close()
		}
	}
	store.session.Close()
	store.ticker.Stop()
}

// GetObjectsToActivate returns inactive objects that are ready to be activated
func (store *MongoStorage) GetObjectsToActivate() ([]common.MetaData, []string, common.SyncServiceError) {
	currentTime := time.Now().Format(time.RFC3339)
	query := bson.M{"$or": []bson.M{
		bson.M{"status": common.NotReadyToSend},
		bson.M{"status": common.ReadyToSend}},
		"metadata.inactive": true,
		"$and": []bson.M{
			bson.M{"metadata.activation-time": bson.M{"$ne": ""}},
			bson.M{"metadata.activation-time": bson.M{"$lte": currentTime}}}}
	selector := bson.M{"metadata": bson.ElementDocument, "status": bson.ElementString}
	result := []object{}
	if err := store.fetchAll(objects, query, selector, &result); err != nil {
		return nil, nil, err
	}

	metaDatas := make([]common.MetaData, len(result))
	statuses := make([]string, len(result))
	for i, r := range result {
		metaDatas[i] = r.MetaData
		statuses[i] = r.Status
	}
	return metaDatas, statuses, nil
}

// StoreObject stores an object
func (store *MongoStorage) StoreObject(metaData common.MetaData, data []byte, status string) common.SyncServiceError {
	id := getObjectCollectionID(metaData)
	if data != nil {
		if err := store.storeDataInFile(id, data); err != nil {
			return err
		}
	} else if !metaData.MetaOnly || metaData.NoData {
		store.removeFile(id)
	}

	var dests []common.StoreDestinationStatus
	if status == common.NotReadyToSend || status == common.ReadyToSend {
		// The object was receieved from a service, i.e. this node is the origin of the object:
		// set its instance id and create destinations array
		metaData.InstanceID = time.Now().UnixNano()

		var err error
		dests, err = store.createDestinations(metaData)
		if err != nil {
			return err
		}
	}

	newObject := object{ID: id, MetaData: metaData, Status: status, RemainingConsumers: metaData.ExpectedConsumers,
		RemainingReceivers: metaData.ExpectedConsumers, Destinations: dests}
	if err := store.upsert(objects, bson.M{"_id": id, "metadata.destination-org-id": metaData.DestOrgID}, newObject); err != nil {
		return &Error{fmt.Sprintf("Failed to store an object. Error: %s.", err)}
	}

	return nil
}

// GetObjectDestinations gets destinations that the object has to be sent to
func (store *MongoStorage) GetObjectDestinations(metaData common.MetaData) ([]common.Destination, common.SyncServiceError) {
	result := object{}
	id := getObjectCollectionID(metaData)
	if err := store.fetchOne(objects, bson.M{"_id": id}, bson.M{"destinations": bson.ElementArray}, &result); err != nil {
		switch err {
		case mgo.ErrNotFound:
			return nil, nil
		default:
			return nil, &Error{fmt.Sprintf("Failed to retrieve object's destinations. Error: %s.", err)}
		}
	}
	dests := make([]common.Destination, 0)
	for _, d := range result.Destinations {
		dests = append(dests, d.Destination)
	}
	return dests, nil
}

// GetObjectDestinationsList gets destinations that the object has to be sent to and their status
func (store *MongoStorage) GetObjectDestinationsList(orgID string, objectType string,
	objectID string) ([]common.StoreDestinationStatus, common.SyncServiceError) {
	result := object{}
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.fetchOne(objects, bson.M{"_id": id}, bson.M{"destinations": bson.ElementArray}, &result); err != nil {
		switch err {
		case mgo.ErrNotFound:
			return nil, nil
		default:
			return nil, &Error{fmt.Sprintf("Failed to retrieve object's destinations. Error: %s.", err)}
		}
	}
	dests := make([]common.StoreDestinationStatus, 0)
	for _, d := range result.Destinations {
		dests = append(dests, d)
	}
	return dests, nil
}

// UpdateObjectDeliveryStatus changes the object's delivery status and message for the destination
func (store *MongoStorage) UpdateObjectDeliveryStatus(status string, message string, orgID string, objectType string, objectID string,
	destType string, destID string) common.SyncServiceError {
	if status == "" && message == "" {
		return nil
	}
	result := object{}
	id := createObjectCollectionID(orgID, objectType, objectID)

	for i := 0; i < maxUpdateTries; i++ {
		if err := store.fetchOne(objects, bson.M{"_id": id},
			bson.M{"metadata": bson.ElementDocument, "destinations": bson.ElementArray, "last-update": bson.ElementTimestamp},
			&result); err != nil {
			return &Error{fmt.Sprintf("Failed to retrieve object. Error: %s.", err)}
		}
		found := false
		allConsumed := true
		for i, d := range result.Destinations {
			if !found && d.Destination.DestType == destType && d.Destination.DestID == destID {
				if message != "" || d.Status == common.Error {
					d.Message = message
				}
				if status != "" {
					d.Status = status
				}
				found = true
				result.Destinations[i] = d
			} else if d.Status != common.Consumed {
				allConsumed = false
			}
		}
		if !found {
			return &Error{"Failed to find destination."}
		}

		query := bson.M{
			"$set":         bson.M{"destinations": result.Destinations},
			"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
		}
		if result.MetaData.AutoDelete && status == common.Consumed && allConsumed && result.MetaData.Expiration == "" {
			// Delete the object by setting its expiration time to one hour
			expirationTime := time.Now().Add(time.Hour * time.Duration(1)).Format(time.RFC3339)
			query = bson.M{
				"$set":         bson.M{"destinations": result.Destinations, "metadata.expiration": expirationTime},
				"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
			}
		}
		if err := store.update(objects, bson.M{"_id": id, "last-update": result.LastUpdate}, query); err != nil {
			if err == mgo.ErrNotFound {
				continue
			}
			return &Error{fmt.Sprintf("Failed to update object's destinations. Error: %s.", err)}
		}
		return nil
	}
	return &Error{fmt.Sprintf("Failed to update object's destinations.")}
}

// UpdateObjectDelivering marks the object as being delivered to all its destinations
func (store *MongoStorage) UpdateObjectDelivering(orgID string, objectType string, objectID string) common.SyncServiceError {
	result := object{}
	id := createObjectCollectionID(orgID, objectType, objectID)
	for i := 0; i < maxUpdateTries; i++ {
		if err := store.fetchOne(objects, bson.M{"_id": id},
			bson.M{"destinations": bson.ElementArray, "last-update": bson.ElementTimestamp},
			&result); err != nil {
			return &Error{fmt.Sprintf("Failed to retrieve object. Error: %s.", err)}
		}
		for i, d := range result.Destinations {
			d.Status = common.Delivering
			result.Destinations[i] = d
		}
		if err := store.update(objects, bson.M{"_id": id, "last-update": result.LastUpdate},
			bson.M{
				"$set":         bson.M{"destinations": result.Destinations},
				"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
			}); err != nil {
			if err == mgo.ErrNotFound {
				continue
			}
			return &Error{fmt.Sprintf("Failed to update object's destinations. Error: %s.", err)}
		}
		return nil
	}
	return &Error{fmt.Sprintf("Failed to update object's destinations.")}
}

// RetrieveObjectStatus finds the object and return its status
func (store *MongoStorage) RetrieveObjectStatus(orgID string, objectType string, objectID string) (string, common.SyncServiceError) {
	result := object{}
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.fetchOne(objects, bson.M{"_id": id}, bson.M{"status": bson.ElementString}, &result); err != nil {
		switch err {
		case mgo.ErrNotFound:
			return "", nil
		default:
			return "", &Error{fmt.Sprintf("Failed to retrieve object's status. Error: %s.", err)}
		}
	}
	return result.Status, nil
}

// RetrieveObjectRemainingConsumers finds the object and returns the number remaining consumers that
// haven't consumed the object yet (ESS only)
func (store *MongoStorage) RetrieveObjectRemainingConsumers(orgID string, objectType string, objectID string) (int, common.SyncServiceError) {
	result := object{}
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.fetchOne(objects, bson.M{"_id": id}, bson.M{"remaining-consumers": bson.ElementInt32}, &result); err != nil {
		return 0, &Error{fmt.Sprintf("Failed to retrieve object's remaining comsumers. Error: %s.", err)}
	}
	return result.RemainingConsumers, nil
}

// DecrementAndReturnRemainingConsumers decrements the number of remaining consumers of the object
func (store *MongoStorage) DecrementAndReturnRemainingConsumers(orgID string, objectType string, objectID string) (int,
	common.SyncServiceError) {
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.update(objects, bson.M{"_id": id},
		bson.M{
			"$inc":         bson.M{"remaining-consumers": -1},
			"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
		}); err != nil {
		return 0, &Error{fmt.Sprintf("Failed to decrement object's remaining consumers. Error: %s.", err)}
	}
	result := object{}
	if err := store.fetchOne(objects, bson.M{"_id": id}, bson.M{"remaining-consumers": bson.ElementInt32}, &result); err != nil {
		return 0, &Error{fmt.Sprintf("Failed to retrieve object's remaining consumers. Error: %s.", err)}
	}
	return result.RemainingConsumers, nil
}

// DecrementAndReturnRemainingReceivers decrements the number of remaining receivers of the object
func (store *MongoStorage) DecrementAndReturnRemainingReceivers(orgID string, objectType string, objectID string) (int,
	common.SyncServiceError) {
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.update(objects, bson.M{"_id": id},
		bson.M{
			"$inc":         bson.M{"remaining-receivers": -1},
			"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
		}); err != nil {
		return 0, &Error{fmt.Sprintf("Failed to decrement object's remaining receivers. Error: %s.", err)}
	}
	result := object{}
	if err := store.fetchOne(objects, bson.M{"_id": id}, bson.M{"remaining-receivers": bson.ElementInt32}, &result); err != nil {
		return 0, &Error{fmt.Sprintf("Failed to retrieve object's remaining receivers. Error: %s.", err)}
	}
	return result.RemainingReceivers, nil
}

// ResetObjectRemainingConsumers sets the remaining consumers count to the original ExpectedConsumers value
func (store *MongoStorage) ResetObjectRemainingConsumers(orgID string, objectType string, objectID string) common.SyncServiceError {
	id := createObjectCollectionID(orgID, objectType, objectID)
	result := object{}
	if err := store.fetchOne(objects, bson.M{"_id": id}, bson.M{"metadata": bson.ElementDocument}, &result); err != nil {
		return &Error{fmt.Sprintf("Failed to retrieve object. Error: %s.", err)}
	}

	if err := store.update(objects, bson.M{"_id": id},
		bson.M{
			"$set":         bson.M{"remaining-consumers": result.MetaData.ExpectedConsumers},
			"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
		}); err != nil {
		return &Error{fmt.Sprintf("Failed to reset object's remaining comsumers. Error: %s.", err)}
	}
	return nil
}

// RetrieveUpdatedObjects returns the list of all the edge updated objects that are not marked as consumed or received
// If received is true, return objects marked as received
func (store *MongoStorage) RetrieveUpdatedObjects(orgID string, objectType string, received bool) ([]common.MetaData, common.SyncServiceError) {
	result := []object{}
	var query interface{}
	if received {
		query = bson.M{"$or": []bson.M{
			bson.M{"status": common.CompletelyReceived},
			bson.M{"status": common.ObjReceived},
			bson.M{"status": common.ObjDeleted}},
			"metadata.destination-org-id": orgID, "metadata.object-type": objectType}
	} else {
		query = bson.M{"$or": []bson.M{
			bson.M{"status": common.CompletelyReceived},
			bson.M{"status": common.ObjDeleted}},
			"metadata.destination-org-id": orgID, "metadata.object-type": objectType}
	}
	if err := store.fetchAll(objects, query, nil, &result); err != nil {
		switch err {
		case mgo.ErrNotFound:
			return nil, nil
		default:
			return nil, &Error{fmt.Sprintf("Failed to fetch the objects. Error: %s.", err)}
		}
	}

	metaDatas := make([]common.MetaData, len(result))
	for i, r := range result {
		metaDatas[i] = r.MetaData
	}
	return metaDatas, nil
}

// RetrieveObjects returns the list of all the objects that need to be sent to the destination.
// Adds the new destination to the destinations lists of the relevant objects.
func (store *MongoStorage) RetrieveObjects(orgID string, destType string, destID string) ([]common.MetaData, common.SyncServiceError) {
	result := []object{}
	query := bson.M{"metadata.destination-org-id": orgID,
		"$or": []bson.M{
			bson.M{"status": common.ReadyToSend},
			bson.M{"status": common.NotReadyToSend},
		}}

OUTER:
	for i := 0; i < maxUpdateTries; i++ {
		if err := store.fetchAll(objects, query, nil, &result); err != nil {
			switch err {
			case mgo.ErrNotFound:
				return nil, nil
			default:
				return nil, &Error{fmt.Sprintf("Failed to fetch the objects. Error: %s.", err)}
			}
		}

		metaDatas := make([]common.MetaData, 0)
		for _, r := range result {
			if (r.MetaData.DestType == "" || r.MetaData.DestType == destType) &&
				(r.MetaData.DestID == "" || r.MetaData.DestID == destID) {
				status := common.Pending
				if r.Status == common.ReadyToSend && !r.MetaData.Inactive {
					metaDatas = append(metaDatas, r.MetaData)
					status = common.Delivering
				}
				// Add destination
				if dest, err := store.RetrieveDestination(orgID, destType, destID); err == nil {
					r.Destinations = append(r.Destinations, common.StoreDestinationStatus{Destination: *dest, Status: status})
					id := createObjectCollectionID(orgID, r.MetaData.ObjectType, r.MetaData.ObjectID)
					if err := store.update(objects, bson.M{"_id": id, "last-update": r.LastUpdate},
						bson.M{
							"$set":         bson.M{"destinations": r.Destinations},
							"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
						}); err != nil {
						if err == mgo.ErrNotFound {
							continue OUTER
						}
						return nil, &Error{fmt.Sprintf("Failed to update object's destinations. Error: %s.", err)}
					}
				}
			}
		}
		return metaDatas, nil
	}
	return nil, &Error{fmt.Sprintf("Failed to update object's destinations.")}
}

// RetrieveObject returns the object meta data with the specified parameters
func (store *MongoStorage) RetrieveObject(orgID string, objectType string, objectID string) (*common.MetaData, common.SyncServiceError) {
	result := object{}
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.fetchOne(objects, bson.M{"_id": id}, bson.M{"metadata": bson.ElementDocument}, &result); err != nil {
		switch err {
		case mgo.ErrNotFound:
			return nil, nil
		default:
			return nil, &Error{fmt.Sprintf("Failed to fetch the object. Error: %s.", err)}
		}
	}
	return &result.MetaData, nil
}

// RetrieveObjectAndStatus returns the object meta data and status with the specified parameters
func (store *MongoStorage) RetrieveObjectAndStatus(orgID string, objectType string, objectID string) (*common.MetaData, string, common.SyncServiceError) {
	result := object{}
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.fetchOne(objects, bson.M{"_id": id}, nil, &result); err != nil {
		switch err {
		case mgo.ErrNotFound:
			return nil, "", nil
		default:
			return nil, "", &Error{fmt.Sprintf("Failed to fetch the object. Error: %s.", err)}
		}
	}
	return &result.MetaData, result.Status, nil
}

// RetrieveObjectData returns the object data with the specified parameters
func (store *MongoStorage) RetrieveObjectData(orgID string, objectType string, objectID string) (io.Reader, common.SyncServiceError) {
	id := createObjectCollectionID(orgID, objectType, objectID)
	fileHandle, err := store.openFile(id)
	if err != nil {
		switch err {
		case mgo.ErrNotFound:
			return nil, nil
		default:
			return nil, &Error{fmt.Sprintf("Failed to open file to read the data. Error: %s.", err)}
		}
	}
	store.putFileHandle(id, fileHandle)
	return fileHandle.file, nil
}

// CloseDataReader closes the data reader if necessary
func (store *MongoStorage) CloseDataReader(dataReader io.Reader) common.SyncServiceError {
	switch v := dataReader.(type) {
	case *mgo.GridFile:
		err := v.Close()
		if id, ok := v.Id().(string); ok {
			if fileHandle := store.getFileHandle(id); fileHandle != nil {
				store.deleteFileHandle(id)
			}
		}
		return err
	default:
		return nil
	}
}

// ReadObjectData returns the object data with the specified parameters
func (store *MongoStorage) ReadObjectData(orgID string, objectType string, objectID string, size int, offset int64) ([]byte, bool, int, common.SyncServiceError) {
	id := createObjectCollectionID(orgID, objectType, objectID)
	fileHandle, err := store.openFile(id)
	if err != nil {
		return nil, true, 0, &Error{fmt.Sprintf("Failed to open file to read the data. Error: %s.", err)}
	}

	offset64 := int64(offset)
	if offset64 >= fileHandle.file.Size() {
		fileHandle.file.Close()
		return make([]byte, 0), true, 0, nil
	}

	_, err = fileHandle.file.Seek(offset64, 0)
	if err != nil {
		fileHandle.file.Close()
		return nil, true, 0, &Error{fmt.Sprintf("Failed to read the data. Error: %s.", err)}
	}
	s := int64(size)
	if s > fileHandle.file.Size()-offset64 {
		s = fileHandle.file.Size() - offset64
	}
	b := make([]byte, s)
	n, err := fileHandle.file.Read(b)
	if err != nil {
		fileHandle.file.Close()
		return nil, true, 0, &Error{fmt.Sprintf("Failed to read the data. Error: %s.", err)}
	}
	if err = fileHandle.file.Close(); err != nil {
		return nil, true, 0, &Error{fmt.Sprintf("Failed to close the file. Error: %s.", err)}
	}
	eof := false
	if fileHandle.file.Size()-offset64 == int64(n) {
		eof = true
	}

	return b, eof, n, nil
}

// StoreObjectData stores object's data
// Return true if the object was found and updated
// Return false and no error, if the object doesn't exist
func (store *MongoStorage) StoreObjectData(orgID string, objectType string, objectID string, dataReader io.Reader) (bool, common.SyncServiceError) {
	id := createObjectCollectionID(orgID, objectType, objectID)
	result := object{}
	if err := store.fetchOne(objects, bson.M{"_id": id}, bson.M{"status": bson.ElementString}, &result); err != nil {
		switch err {
		case mgo.ErrNotFound:
			return false, nil
		default:
			return false, &Error{fmt.Sprintf("Failed to store the data. Error: %s.", err)}
		}
	}
	if result.Status == common.NotReadyToSend {
		store.UpdateObjectStatus(orgID, objectType, objectID, common.ReadyToSend)
	} else if result.Status == common.ReadyToSend {
		// The data is being updated, set the instance id
		if err := store.update(objects, bson.M{"_id": id},
			bson.M{
				"$set":         bson.M{"metadata.instance-id": time.Now().UnixNano()},
				"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
			}); err != nil {
			return false, &Error{fmt.Sprintf("Failed to set instance id. Error: %s.", err)}
		}
	}

	_, size, err := store.copyDataToFile(id, dataReader, true, true)
	if err != nil {
		return false, err
	}

	// Update object size
	if err := store.update(objects, bson.M{"_id": id}, bson.M{"$set": bson.M{"metadata.object-size": size}}); err != nil {
		return false, &Error{fmt.Sprintf("Failed to update object's size. Error: %s.", err)}
	}

	return true, nil
}

// AppendObjectData appends a chunk of data to the object's data
func (store *MongoStorage) AppendObjectData(orgID string, objectType string, objectID string, dataReader io.Reader,
	dataLength uint32, offset int64, total int64, isFirstChunk bool, isLastChunk bool) common.SyncServiceError {
	id := createObjectCollectionID(orgID, objectType, objectID)
	var fileHandle *fileHandle
	if isFirstChunk {
		store.removeFile(id)
		fh, err := store.createFile(id)
		if err != nil {
			return err
		}
		fileHandle = fh
	} else {
		fh := store.getFileHandle(id)
		if fh == nil {
			return &Error{fmt.Sprintf("Failed to append the data at offset %d, the file %s doesn't exist.", offset, id)}
		}
		fileHandle = fh
	}

	var n int
	var err error
	var data []byte
	if dataLength > 0 {
		data = make([]byte, dataLength)
		n, err = dataReader.Read(data)
	} else {
		data, err = ioutil.ReadAll(dataReader)
		n = len(data)
	}
	if err != nil {
		return &Error{fmt.Sprintf("Failed to read the data from the dataReader. Error: %s.", err)}
	}
	if uint32(n) != dataLength && dataLength > 0 {
		return &Error{fmt.Sprintf("Failed to read all the data from the dataReader. Read %d instead of %d.", n, dataLength)}
	}
	if offset == fileHandle.offset {
		for {
			if trace.IsLogging(logger.TRACE) {
				trace.Trace(" Put data (%d) in file at offset %d\n", len(data), fileHandle.offset)
			}
			n, err = fileHandle.file.Write(data)
			if err != nil {
				return &Error{fmt.Sprintf("Failed to write the data to the file. Error: %s.", err)}
			}
			if n != len(data) {
				return &Error{fmt.Sprintf("Failed to write all the data to the file. Wrote %d instead of %d.", n, len(data))}
			}
			fileHandle.offset += int64(n)
			if fileHandle.chunks == nil {
				break
			}
			data = fileHandle.chunks[fileHandle.offset]
			if data == nil {
				break
			}
			delete(fileHandle.chunks, fileHandle.offset)
			if trace.IsLogging(logger.TRACE) {
				trace.Trace(" Get data (%d) from map at offset %d\n", len(data), fileHandle.offset)
			}
		}
	} else {
		if fileHandle.chunks == nil {
			fileHandle.chunks = make(map[int64][]byte)
		}
		if len(fileHandle.chunks) > 100 {
			if trace.IsLogging(logger.INFO) {
				trace.Info(" Discard data chunk at offset %d since there are too many (%d) out-of-order chunks\n", offset, len(fileHandle.chunks))
			}
			return &Discarded{fmt.Sprintf(" Discard data chunk at offset %d since there are too many out-of-order chunks\n", offset)}
		}
		fileHandle.chunks[offset] = data
		if trace.IsLogging(logger.TRACE) {
			trace.Trace(" Put data (%d) in map at offset %d (# in map %d)\n", len(data), offset, len(fileHandle.chunks))
		}
	}
	if isLastChunk {
		store.deleteFileHandle(id)
		err := fileHandle.file.Close()
		if err != nil {
			return &Error{fmt.Sprintf("Failed to close the file. Error: %s.", err)}
		}
	} else {
		store.putFileHandle(id, fileHandle)
	}

	return nil
}

// UpdateObjectStatus updates object's status
func (store *MongoStorage) UpdateObjectStatus(orgID string, objectType string, objectID string, status string) common.SyncServiceError {
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.update(objects, bson.M{"_id": id},
		bson.M{
			"$set":         bson.M{"status": status},
			"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
		}); err != nil {
		return &Error{fmt.Sprintf("Failed to update object's status. Error: %s.", err)}
	}
	return nil
}

// UpdateObjectSourceDataURI updates object's source data URI
func (store *MongoStorage) UpdateObjectSourceDataURI(orgID string, objectType string, objectID string, sourceDataURI string) common.SyncServiceError {
	return nil
}

// MarkObjectDeleted marks the object as deleted
func (store *MongoStorage) MarkObjectDeleted(orgID string, objectType string, objectID string) common.SyncServiceError {
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.update(objects, bson.M{"_id": id},
		bson.M{
			"$set":         bson.M{"status": common.ObjDeleted, "metadata.deleted": true},
			"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
		}); err != nil {
		return &Error{fmt.Sprintf("Failed to mark object as deleted. Error: %s.", err)}
	}
	return nil
}

// ActivateObject marks object as active
func (store *MongoStorage) ActivateObject(orgID string, objectType string, objectID string) common.SyncServiceError {
	id := createObjectCollectionID(orgID, objectType, objectID)
	if err := store.update(objects, bson.M{"_id": id},
		bson.M{"$set": bson.M{"metadata.inactive": false},
			"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
		}); err != nil {
		return &Error{fmt.Sprintf("Failed to mark object as active. Error: %s.", err)}
	}
	return nil
}

// DeleteStoredObject deletes the object
func (store *MongoStorage) DeleteStoredObject(orgID string, objectType string, objectID string) common.SyncServiceError {
	id := createObjectCollectionID(orgID, objectType, objectID)
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Deleting object %s\n", id)
	}
	if err := store.removeFile(id); err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Error in deleteStoredObject: failed to delete data file. Error: %s\n", err)
		}
	}
	if err := store.removeAll(objects, bson.M{"_id": id}); err != nil {
		if err == mgo.ErrNotFound {
			return nil
		}
		return &Error{fmt.Sprintf("Failed to delete object. Error: %s.", err)}
	}
	return nil
}

// DeleteStoredData deletes the object's data
func (store *MongoStorage) DeleteStoredData(orgID string, objectType string, objectID string) common.SyncServiceError {
	id := createObjectCollectionID(orgID, objectType, objectID)
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Deleting object's data %s\n", id)
	}
	if err := store.removeFile(id); err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Error in DeleteStoredData: failed to delete data file. Error: %s\n", err)
		}
		return err
	}
	return nil
}

// AddWebhook stores a webhook for an object type
func (store *MongoStorage) AddWebhook(orgID string, objectType string, url string) common.SyncServiceError {
	id := orgID + ":" + objectType
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Adding a webhook for %s\n", id)
	}
	result := &webhookObject{}
	for i := 0; i < maxUpdateTries; i++ {
		if err := store.fetchOne(webhooks, bson.M{"_id": id}, nil, &result); err != nil {
			if err == mgo.ErrNotFound {
				result.Hooks = make([]string, 0)
				result.Hooks = append(result.Hooks, url)
				result.ID = id
				if err = store.insert(webhooks, result); err != nil {
					if mgo.IsDup(err) {
						continue
					}
					return &Error{fmt.Sprintf("Failed to insert a webhook. Error: %s.", err)}
				}
				return nil
			}
			return &Error{fmt.Sprintf("Failed to add a webhook. Error: %s.", err)}
		}

		// Don't add the webhook if it already is in the list
		for _, hook := range result.Hooks {
			if url == hook {
				return nil
			}
		}
		result.Hooks = append(result.Hooks, url)
		if err := store.update(webhooks, bson.M{"_id": id, "last-update": result.LastUpdate},
			bson.M{
				"$set":         bson.M{"hooks": result.Hooks},
				"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
			}); err != nil {
			if err == mgo.ErrNotFound {
				continue
			}
			return &Error{fmt.Sprintf("Failed to add a webhook. Error: %s.", err)}
		}
		return nil
	}
	return &Error{fmt.Sprintf("Failed to add a webhook.")}
}

// DeleteWebhook deletes a webhook for an object type
func (store *MongoStorage) DeleteWebhook(orgID string, objectType string, url string) common.SyncServiceError {
	id := orgID + ":" + objectType
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Deleting a webhook for %s\n", id)
	}
	result := &webhookObject{}
	for i := 0; i < maxUpdateTries; i++ {
		if err := store.fetchOne(webhooks, bson.M{"_id": id}, nil, &result); err != nil {
			return &Error{fmt.Sprintf("Failed to delete a webhook. Error: %s.", err)}
		}
		deleted := false
		for i, hook := range result.Hooks {
			if strings.EqualFold(hook, url) {
				result.Hooks[i] = result.Hooks[len(result.Hooks)-1]
				result.Hooks = result.Hooks[:len(result.Hooks)-1]
				deleted = true
				break
			}
		}
		if !deleted {
			return nil
		}
		if err := store.update(webhooks, bson.M{"_id": id, "last-update": result.LastUpdate},
			bson.M{
				"$set":         bson.M{"hooks": result.Hooks},
				"$currentDate": bson.M{"last-update": bson.M{"$type": "timestamp"}},
			}); err != nil {
			if err == mgo.ErrNotFound {
				continue
			}
			return &Error{fmt.Sprintf("Failed to delete a webhook. Error: %s.", err)}
		}
		return nil
	}
	return &Error{fmt.Sprintf("Failed to delete a webhook.")}
}

// RetrieveWebhooks gets the webhooks for the object type
func (store *MongoStorage) RetrieveWebhooks(orgID string, objectType string) ([]string, common.SyncServiceError) {
	id := orgID + ":" + objectType
	if trace.IsLogging(logger.TRACE) {
		trace.Trace("Retrieving a webhook for %s\n", id)
	}
	result := &webhookObject{}
	if err := store.fetchOne(webhooks, bson.M{"_id": id}, nil, &result); err != nil {
		return nil, err
	}
	if len(result.Hooks) == 0 {
		return nil, &NotFound{"No webhooks"}
	}
	return result.Hooks, nil
}

// RetrieveDestinations returns all the destinations with the provided orgID and destType
func (store *MongoStorage) RetrieveDestinations(orgID string, destType string) ([]common.Destination, common.SyncServiceError) {
	result := []destinationObject{}
	var err error

	if orgID == "" {
		if destType == "" {
			err = store.fetchAll(destinations, nil, nil, &result)
		} else {
			err = store.fetchAll(destinations, bson.M{"destination.destination-type": destType}, nil, &result)
		}
	} else {
		if destType == "" {
			err = store.fetchAll(destinations, bson.M{"destination.destination-org-id": orgID}, nil, &result)
		} else {
			err = store.fetchAll(destinations, bson.M{"destination.destination-org-id": orgID, "destination.destination-type": destType}, nil, &result)
		}
	}
	if err != nil && err != mgo.ErrNotFound {
		return nil, &Error{fmt.Sprintf("Failed to fetch the destinations. Error: %s.", err)}
	}

	dests := make([]common.Destination, len(result))
	for i, r := range result {
		dests[i] = r.Destination
	}
	return dests, nil
}

// DestinationExists returns true if the destination exists, and false otherwise
func (store *MongoStorage) DestinationExists(orgID string, destType string, destID string) (bool, common.SyncServiceError) {
	result := destinationObject{}
	id := createDestinationCollectionID(orgID, destType, destID)
	if err := store.fetchOne(destinations, bson.M{"_id": id}, nil, &result); err != nil {
		if err == mgo.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// StoreDestination stores the destination
func (store *MongoStorage) StoreDestination(destination common.Destination) common.SyncServiceError {
	id := getDestinationCollectionID(destination)
	newObject := destinationObject{ID: id, Destination: destination}
	err := store.upsert(destinations, bson.M{"_id": id, "destination.destination-org-id": destination.DestOrgID}, newObject)
	if err != nil {
		return &Error{fmt.Sprintf("Failed to store a destination. Error: %s.", err)}
	}
	return nil
}

// DeleteDestination deletes the destination
func (store *MongoStorage) DeleteDestination(orgID string, destType string, destID string) common.SyncServiceError {
	id := createDestinationCollectionID(orgID, destType, destID)
	if err := store.removeAll(destinations, bson.M{"_id": id}); err != nil {
		return &Error{fmt.Sprintf("Failed to delete destination. Error: %s.", err)}
	}
	return nil
}

// RetrieveDestinationProtocol retrieves the communication protocol for the destination
func (store *MongoStorage) RetrieveDestinationProtocol(orgID string, destType string, destID string) (string, common.SyncServiceError) {
	result := destinationObject{}
	id := createDestinationCollectionID(orgID, destType, destID)
	if err := store.fetchOne(destinations, bson.M{"_id": id}, nil, &result); err != nil {
		return "", &Error{fmt.Sprintf("Failed to fetch the destination. Error: %s.", err)}
	}
	return result.Destination.Communication, nil
}

// RetrieveDestination retrieves a destination
func (store *MongoStorage) RetrieveDestination(orgID string, destType string, destID string) (*common.Destination, common.SyncServiceError) {
	result := destinationObject{}
	id := createDestinationCollectionID(orgID, destType, destID)
	if err := store.fetchOne(destinations, bson.M{"_id": id}, nil, &result); err != nil {
		return nil, &Error{fmt.Sprintf("Failed to fetch the destination. Error: %s.", err)}
	}
	return &result.Destination, nil
}

// GetObjectsForDestination retrieves objects that are in use on a given node
func (store *MongoStorage) GetObjectsForDestination(orgID string, destType string, destID string) ([]common.ObjectStatus, common.SyncServiceError) {
	notificationRecords := []notificationObject{}
	query := bson.M{"$or": []bson.M{
		bson.M{"notification.status": common.Update},
		bson.M{"notification.status": common.UpdatePending},
		bson.M{"notification.status": common.Updated},
		bson.M{"notification.status": common.ReceivedByDestination},
		bson.M{"notification.status": common.ConsumedByDestination},
		bson.M{"notification.status": common.Error}},
		"notification.destination-org-id": orgID,
		"notification.destination-id":     destID,
		"notification.destination-type":   destType}

	if err := store.fetchAll(notifications, query, nil, &notificationRecords); err != nil && err != mgo.ErrNotFound {
		return nil, &Error{fmt.Sprintf("Failed to fetch the notifications. Error: %s.", err)}
	}

	var status string
	objectStatuses := make([]common.ObjectStatus, 0)
	for _, n := range notificationRecords {
		switch n.Notification.Status {
		case common.Update:
			status = common.Delivering
		case common.UpdatePending:
			status = common.Delivering
		case common.Updated:
			status = common.Delivering
		case common.ReceivedByDestination:
			status = common.Delivered
		case common.ConsumedByDestination:
			status = common.Consumed
		case common.Error:
			status = common.Error
		}
		objectStatus := common.ObjectStatus{OrgID: orgID, ObjectType: n.Notification.ObjectType, ObjectID: n.Notification.ObjectID, Status: status}
		objectStatuses = append(objectStatuses, objectStatus)
	}
	return objectStatuses, nil
}

// UpdateNotificationRecord updates/adds a notification record to the object
func (store *MongoStorage) UpdateNotificationRecord(notification common.Notification) common.SyncServiceError {
	id := getNotificationCollectionID(&notification)
	if notification.ResendTime == 0 {
		resendTime := time.Now().Unix() + int64(common.Configuration.ResendInterval*6)
		notification.ResendTime = resendTime
	}
	n := notificationObject{ID: id, Notification: notification}
	err := store.upsert(notifications,
		bson.M{
			"_id": id,
			"notification.destination-org-id": notification.DestOrgID,
			"notification.destination-id":     notification.DestID,
			"notification.destination-type":   notification.DestType,
		},
		n)
	if err != nil {
		return &Error{fmt.Sprintf("Failed to update notification record. Error: %s.", err)}
	}
	return nil
}

// UpdateNotificationResendTime sets the resend time of the notification to common.Configuration.ResendInterval*6
func (store *MongoStorage) UpdateNotificationResendTime(notification common.Notification) common.SyncServiceError {
	id := getNotificationCollectionID(&notification)
	resendTime := time.Now().Unix() + int64(common.Configuration.ResendInterval*6)
	if err := store.update(notifications, bson.M{"_id": id}, bson.M{"$set": bson.M{"notification.resend-time": resendTime}}); err != nil {
		return &Error{fmt.Sprintf("Failed to update notification resend time. Error: %s.", err)}
	}
	return nil
}

// RetrieveNotificationRecord retrieves notification
func (store *MongoStorage) RetrieveNotificationRecord(orgID string, objectType string, objectID string, destType string,
	destID string) (*common.Notification, common.SyncServiceError) {
	id := createNotificationCollectionID(orgID, objectType, objectID, destType, destID)
	result := notificationObject{}
	if err := store.fetchOne(notifications, bson.M{"_id": id}, nil, &result); err != nil {
		return nil, &Error{fmt.Sprintf("Failed to fetch the notification. Error: %s.", err)}
	}
	return &result.Notification, nil
}

// DeleteNotificationRecords deletes notification records to an object
func (store *MongoStorage) DeleteNotificationRecords(orgID string, objectType string, objectID string, destType string, destID string) common.SyncServiceError {
	var err error
	if objectType != "" && objectID != "" {
		if destType != "" && destID != "" {
			id := createNotificationCollectionID(orgID, objectType, objectID, destType, destID)
			err = store.removeAll(notifications, bson.M{"_id": id})
		} else {
			err = store.removeAll(notifications,
				bson.M{"notification.destination-org-id": orgID, "notification.object-type": objectType,
					"notification.object-id": objectID})
		}
	} else {
		err = store.removeAll(notifications,
			bson.M{"notification.destination-org-id": orgID, "notification.destination-type": destType,
				"notification.destination-id": destID})
	}

	if err != nil && err != mgo.ErrNotFound {
		return &Error{fmt.Sprintf("Failed to delete notification records. Error: %s.", err)}
	}
	return nil
}

// RetrieveNotifications returns the list of all the notifications that need to be resent to the destination
func (store *MongoStorage) RetrieveNotifications(orgID string, destType string, destID string, retrieveReceived bool) ([]common.Notification, common.SyncServiceError) {
	result := []notificationObject{}
	var query bson.M
	if destType == "" && destID == "" {
		currentTime := time.Now().Unix()

		query = bson.M{"$or": []bson.M{
			bson.M{"notification.status": common.Getdata},
			bson.M{
				"notification.resend-time": bson.M{"$lte": currentTime},
				"$or": []bson.M{
					bson.M{"notification.status": common.Update},
					bson.M{"notification.status": common.Received},
					bson.M{"notification.status": common.Consumed},
					bson.M{"notification.status": common.Data},
					bson.M{"notification.status": common.Delete},
					bson.M{"notification.status": common.Deleted}}}}}
	} else {
		if retrieveReceived {
			query = bson.M{"$or": []bson.M{
				bson.M{"notification.status": common.Update},
				bson.M{"notification.status": common.Received},
				bson.M{"notification.status": common.Consumed},
				bson.M{"notification.status": common.Getdata},
				bson.M{"notification.status": common.Data},
				bson.M{"notification.status": common.ReceivedByDestination},
				bson.M{"notification.status": common.Delete},
				bson.M{"notification.status": common.Deleted}},
				"notification.destination-org-id": orgID,
				"notification.destination-id":     destID,
				"notification.destination-type":   destType}
		} else {
			query = bson.M{"$or": []bson.M{
				bson.M{"notification.status": common.Update},
				bson.M{"notification.status": common.Received},
				bson.M{"notification.status": common.Consumed},
				bson.M{"notification.status": common.Getdata},
				bson.M{"notification.status": common.Delete},
				bson.M{"notification.status": common.Deleted}},
				"notification.destination-org-id": orgID,
				"notification.destination-id":     destID,
				"notification.destination-type":   destType}
		}
	}
	if err := store.fetchAll(notifications, query, nil, &result); err != nil && err != mgo.ErrNotFound {
		return nil, &Error{fmt.Sprintf("Failed to fetch the notifications. Error: %s.", err)}
	}

	notifications := make([]common.Notification, 0)
	for _, n := range result {
		notifications = append(notifications, n.Notification)
	}
	return notifications, nil
}

// RetrievePendingNotifications returns the list of pending notifications that are waiting to be sent to the destination
func (store *MongoStorage) RetrievePendingNotifications(orgID string, destType string, destID string) ([]common.Notification, common.SyncServiceError) {
	result := []notificationObject{}
	var query bson.M

	if destType == "" && destID == "" {
		query = bson.M{"$or": []bson.M{
			bson.M{"notification.status": common.UpdatePending},
			bson.M{"notification.status": common.ConsumedPending},
			bson.M{"notification.status": common.DeletePending},
			bson.M{"notification.status": common.DeletedPending}},
			"notification.destination-org-id": orgID}
	} else {
		query = bson.M{"$or": []bson.M{
			bson.M{"notification.status": common.UpdatePending},
			bson.M{"notification.status": common.ConsumedPending},
			bson.M{"notification.status": common.DeletePending},
			bson.M{"notification.status": common.DeletedPending}},
			"notification.destination-org-id": orgID,
			"notification.destination-id":     destID,
			"notification.destination-type":   destType}
	}
	if err := store.fetchAll(notifications, query, nil, &result); err != nil && err != mgo.ErrNotFound {
		return nil, &Error{fmt.Sprintf("Failed to fetch the notifications. Error: %s.", err)}
	}

	notifications := make([]common.Notification, 0)
	for _, n := range result {
		notifications = append(notifications, n.Notification)
	}
	return notifications, nil
}

// InsertInitialLeader inserts the initial leader document if the collection is empty
func (store *MongoStorage) InsertInitialLeader(leaderID string) (bool, common.SyncServiceError) {
	doc := leaderDocument{ID: 1, UUID: leaderID, HeartbeatTimeout: common.Configuration.LeadershipTimeout, Version: 1}
	err := store.insert(leader, doc)

	if err != nil {
		if !mgo.IsDup(err) {
			return false, &Error{fmt.Sprintf("Failed to insert document into syncLeaderElection collection. Error: %s\n", err)}
		}
		return false, nil
	}

	return true, nil
}

// LeaderPeriodicUpdate does the periodic update of the leader document by the leader
func (store *MongoStorage) LeaderPeriodicUpdate(leaderID string) (bool, common.SyncServiceError) {
	err := store.update(leader,
		bson.M{"_id": 1, "uuid": leaderID},
		bson.M{"$currentDate": bson.M{"last-heartbeat-ts": bson.M{"$type": "timestamp"}}},
	)
	if err != nil {
		if mgo.ErrNotFound != err {
			return false, &Error{fmt.Sprintf("Failed to update the document in the syncLeaderElection collection. Error: %s\n", err)}
		}
		return false, nil
	}

	return true, nil
}

// RetrieveLeader retrieves the Heartbeat timeout and Last heartbeat time stamp from the leader document
func (store *MongoStorage) RetrieveLeader() (string, int32, time.Time, int64, common.SyncServiceError) {
	doc := leaderDocument{}
	err := store.fetchOne(leader, bson.M{"_id": 1}, nil, &doc)
	if err != nil {
		return "", 0, time.Now(), 0, &Error{fmt.Sprintf("Failed to fetch the document in the syncLeaderElection collection. Error: %s", err)}
	}
	return doc.UUID, doc.HeartbeatTimeout, doc.LastHeartbeatTS.Time(), doc.Version, nil
}

// UpdateLeader updates the leader entry for a leadership takeover
func (store *MongoStorage) UpdateLeader(leaderID string, version int64) (bool, common.SyncServiceError) {
	err := store.update(leader,
		bson.M{"_id": 1, "version": version},
		bson.M{
			"$currentDate": bson.M{"last-heartbeat-ts": bson.M{"$type": "timestamp"}},
			"$set": bson.M{
				"uuid":              leaderID,
				"heartbeat-timeout": common.Configuration.LeadershipTimeout,
				"version":           version + 1,
			},
		},
	)
	if err != nil {
		if err != mgo.ErrNotFound {
			// Only complain if someone else didn't steal the leadership
			return false, &Error{fmt.Sprintf("Failed to update the document in the syncLeaderElection collection. Error: %s\n", err)}
		}
		return false, nil
	}
	return true, nil
}

// ResignLeadership causes this sync service to give up the Leadership
func (store *MongoStorage) ResignLeadership(leaderID string) common.SyncServiceError {
	timestamp, err := bson.NewMongoTimestamp(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), 1)
	if err != nil {
		return err
	}
	err = store.update(leader,
		bson.M{"_id": 1, "uuid": leaderID},
		bson.M{
			"$set": bson.M{
				"last-heartbeat-ts": timestamp,
			},
		},
	)
	if err != nil && mgo.ErrNotFound != err {
		return &Error{fmt.Sprintf("Failed to update the document in the syncLeaderElection collection. Error: %s\n", err)}
	}

	return nil
}

// RetrieveTimeOnServer retrieves the current time on the database server
func (store *MongoStorage) RetrieveTimeOnServer() (time.Time, error) {
	result := isMasterResult{}
	err := store.run("isMaster", &result)
	if err == nil && !result.OK {
		err = &Error{"Failed running isMaster command on MongoDB server"}
	}
	return result.LocalTime, err
}

// StoreOrgToMessagingGroup inserts organization to messaging groups table
func (store *MongoStorage) StoreOrgToMessagingGroup(orgID string, messagingGroup string) common.SyncServiceError {
	object := messagingGroupObject{ID: orgID, GroupName: messagingGroup}
	err := store.upsert(messagingGroups, bson.M{"_id": orgID}, object)
	if err != nil {
		return &Error{fmt.Sprintf("Failed to store organization's messaging group. Error: %s.", err)}
	}
	return nil
}

// DeleteOrgToMessagingGroup deletes organization from messaging groups table
func (store *MongoStorage) DeleteOrgToMessagingGroup(orgID string) common.SyncServiceError {
	if err := store.removeAll(messagingGroups, bson.M{"_id": orgID}); err != nil && err != mgo.ErrNotFound {
		return err
	}
	return nil
}

// RetrieveMessagingGroup retrieves messaging group for organization
func (store *MongoStorage) RetrieveMessagingGroup(orgID string) (string, common.SyncServiceError) {
	result := messagingGroupObject{}
	if err := store.fetchOne(messagingGroups, bson.M{"_id": orgID}, nil, &result); err != nil {
		if err != mgo.ErrNotFound {
			return "", err
		}
		return "", nil
	}
	return result.GroupName, nil
}

// RetrieveUpdatedMessagingGroups retrieves messaging groups that were updated after the specified time
func (store *MongoStorage) RetrieveUpdatedMessagingGroups(time time.Time) ([]common.MessagingGroup,
	common.SyncServiceError) {
	timestamp, err := bson.NewMongoTimestamp(time, 1)
	if err != nil {
		return nil, err
	}
	result := []messagingGroupObject{}
	if err := store.fetchAll(messagingGroups, bson.M{"last-update": bson.M{"$gte": timestamp}}, nil, &result); err != nil {
		return nil, err
	}
	groups := make([]common.MessagingGroup, 0)
	for _, group := range result {
		groups = append(groups, common.MessagingGroup{OrgID: group.ID, GroupName: group.GroupName})
	}
	return groups, nil
}

// DeleteOrganization cleans up the storage from all the records associated with the organization
func (store *MongoStorage) DeleteOrganization(orgID string) common.SyncServiceError {
	if err := store.DeleteOrgToMessagingGroup(orgID); err != nil {
		return err
	}

	if err := store.removeAll(destinations, bson.M{"destination.destination-org-id": orgID}); err != nil && err != mgo.ErrNotFound {
		return &Error{fmt.Sprintf("Failed to delete destinations. Error: %s.", err)}
	}

	if err := store.removeAll(notifications, bson.M{"notification.destination-org-id": orgID}); err != nil && err != mgo.ErrNotFound {
		return &Error{fmt.Sprintf("Failed to delete notifications. Error: %s.", err)}
	}

	if err := store.removeAll(objects, bson.M{"metadata.destination-org-id": orgID}); err != nil && err != mgo.ErrNotFound {
		return &Error{fmt.Sprintf("Failed to delete objects. Error: %s.", err)}
	}

	return nil
}

// IsConnected returns false if the storage cannont be reached, and true otherwise
func (store *MongoStorage) IsConnected() bool {
	return store.connected
}

// StoreOrganization stores organization information
// Returns the stored record timestamp for multiple CSS updates
func (store *MongoStorage) StoreOrganization(org common.Organization) (time.Time, common.SyncServiceError) {
	object := organizationObject{ID: org.OrgID, Organization: org}
	err := store.upsert(organizations, bson.M{"_id": org.OrgID}, object)
	if err != nil {
		return time.Now(), &Error{fmt.Sprintf("Failed to store organization's info. Error: %s.", err)}
	}

	if err := store.fetchOne(organizations, bson.M{"_id": org.OrgID}, nil, &object); err != nil {
		return time.Now(), err
	}

	return object.LastUpdate.Time(), nil
}

// RetrieveOrganizationInfo retrieves organization information
func (store *MongoStorage) RetrieveOrganizationInfo(orgID string) (*common.StoredOrganization, common.SyncServiceError) {
	result := organizationObject{}
	if err := store.fetchOne(organizations, bson.M{"_id": orgID}, nil, &result); err != nil {
		if err != mgo.ErrNotFound {
			return nil, err
		}
		return nil, nil
	}
	return &common.StoredOrganization{Org: result.Organization, Timestamp: result.LastUpdate.Time()}, nil
}

// DeleteOrganizationInfo deletes organization information
func (store *MongoStorage) DeleteOrganizationInfo(orgID string) common.SyncServiceError {
	if err := store.removeAll(organizations, bson.M{"_id": orgID}); err != nil && err != mgo.ErrNotFound {
		return err
	}
	return nil
}

// RetrieveOrganizations retrieves stored organizations' info
func (store *MongoStorage) RetrieveOrganizations() ([]common.StoredOrganization, common.SyncServiceError) {
	result := []organizationObject{}
	if err := store.fetchAll(organizations, nil, nil, &result); err != nil {
		return nil, err
	}
	orgs := make([]common.StoredOrganization, 0)
	for _, org := range result {
		orgs = append(orgs, common.StoredOrganization{Org: org.Organization, Timestamp: org.LastUpdate.Time()})
	}
	return orgs, nil
}

// RetrieveUpdatedOrganizations retrieves organizations that were updated after the specified time
func (store *MongoStorage) RetrieveUpdatedOrganizations(time time.Time) ([]common.StoredOrganization, common.SyncServiceError) {
	timestamp, err := bson.NewMongoTimestamp(time, 1)
	if err != nil {
		return nil, err
	}
	result := []organizationObject{}
	if err := store.fetchAll(organizations, bson.M{"last-update": bson.M{"$gte": timestamp}}, nil, &result); err != nil {
		return nil, err
	}
	orgs := make([]common.StoredOrganization, 0)
	for _, org := range result {
		orgs = append(orgs, common.StoredOrganization{Org: org.Organization, Timestamp: org.LastUpdate.Time()})
	}
	return orgs, nil
}

// AddUsersToACL adds users to an ACL
func (store *MongoStorage) AddUsersToACL(aclType string, orgID string, key string, usernames []string) common.SyncServiceError {
	return store.addUsersToACLHelper(acls, aclType, orgID, key, usernames)
}

// RemoveUsersFromACL removes users from an ACL
func (store *MongoStorage) RemoveUsersFromACL(aclType string, orgID string, key string, usernames []string) common.SyncServiceError {
	return store.removeUsersFromACLHelper(acls, aclType, orgID, key, usernames)
}

// RetrieveACL retrieves the list of usernames on an ACL
func (store *MongoStorage) RetrieveACL(aclType string, orgID string, key string) ([]string, common.SyncServiceError) {
	return store.retrieveACLHelper(acls, aclType, orgID, key)
}

// RetrieveACLsInOrg retrieves the list of ACLs in an organization
func (store *MongoStorage) RetrieveACLsInOrg(aclType string, orgID string) ([]string, common.SyncServiceError) {
	return store.retrieveACLsInOrgHelper(acls, aclType, orgID)
}