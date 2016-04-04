// Copyright (c) 2015 - Max Persson <max@looplab.se>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build mongo

package eventhorizon

import (
	"errors"
	"time"

	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

// ErrCouldNotDialDB returned when the database could not be dialed.
var ErrCouldNotDialDB = errors.New("could not dial database")

// ErrNoDBSession returned when no database session is set.
var ErrNoDBSession = errors.New("no database session")

// ErrCouldNotClearDB returned when the database could not be cleared.
var ErrCouldNotClearDB = errors.New("could not clear database")

// ErrEventNotRegistered returned when an event is not registered.
var ErrEventNotRegistered = errors.New("event not registered")

// ErrModelNotSet returned when an model is not set on a read repository.
var ErrModelNotSet = errors.New("model not set")

// ErrCouldNotMarshalEvent returned when an event could not be marshaled into BSON.
var ErrCouldNotMarshalEvent = errors.New("could not marshal event")

// ErrCouldNotUnmarshalEvent returned when an event could not be unmarshaled into a concrete type.
var ErrCouldNotUnmarshalEvent = errors.New("could not unmarshal event")

// ErrCouldNotLoadAggregate returned when an aggregate could not be loaded.
var ErrCouldNotLoadAggregate = errors.New("could not load aggregate")

// ErrCouldNotSaveAggregate returned when an aggregate could not be saved.
var ErrCouldNotSaveAggregate = errors.New("could not save aggregate")

// ErrInvalidEvent returned when an event does not implement the Event interface.
var ErrInvalidEvent = errors.New("invalid event")

// MongoEventStore implements an EventStore for MongoDB.
type MongoEventStore struct {
	eventBus  EventBus
	session   *mgo.Session
	db        string
	factories map[string]func() Event
}

// NewMongoEventStore creates a new MongoEventStore.
func NewMongoEventStore(eventBus EventBus, url, database string) (*MongoEventStore, error) {
	session, err := mgo.Dial(url)
	if err != nil {
		return nil, ErrCouldNotDialDB
	}

	session.SetMode(mgo.Strong, true)
	session.SetSafe(&mgo.Safe{W: 1})

	return NewMongoEventStoreWithSession(eventBus, session, database)
}

// NewMongoEventStoreWithSession creates a new MongoEventStore with a session.
func NewMongoEventStoreWithSession(eventBus EventBus, session *mgo.Session, database string) (*MongoEventStore, error) {
	if session == nil {
		return nil, ErrNoDBSession
	}

	s := &MongoEventStore{
		eventBus:  eventBus,
		factories: make(map[string]func() Event),
		session:   session,
		db:        database,
	}

	return s, nil
}

type mongoAggregateRecord struct {
	AggregateID string              `bson:"_id"`
	Version     int                 `bson:"version"`
	Events      []*mongoEventRecord `bson:"events"`
	// Type        string        `bson:"type"`
	// Snapshot    bson.Raw      `bson:"snapshot"`
}

type mongoEventRecord struct {
	Type      string    `bson:"type"`
	Version   int       `bson:"version"`
	Timestamp time.Time `bson:"timestamp"`
	Event     Event     `bson:"-"`
	Data      bson.Raw  `bson:"data"`
}

// Save appends all events in the event stream to the database.
func (s *MongoEventStore) Save(events []Event) error {
	if len(events) == 0 {
		return ErrNoEventsToAppend
	}

	sess := s.session.Copy()
	defer sess.Close()

	for _, event := range events {
		// Get an existing aggregate, if any.
		var existing []mongoAggregateRecord
		err := sess.DB(s.db).C("events").FindId(event.AggregateID().String()).
			Select(bson.M{"version": 1}).Limit(1).All(&existing)
		if err != nil || len(existing) > 1 {
			return ErrCouldNotLoadAggregate
		}

		// Marshal event data.
		var data []byte
		if data, err = bson.Marshal(event); err != nil {
			return ErrCouldNotMarshalEvent
		}

		// Create the event record with timestamp.
		r := &mongoEventRecord{
			Type:      event.EventType(),
			Version:   1,
			Timestamp: time.Now(),
			Data:      bson.Raw{3, data},
		}

		// Either insert a new aggregate or append to an existing.
		if len(existing) == 0 {
			aggregate := mongoAggregateRecord{
				AggregateID: event.AggregateID().String(),
				Version:     1,
				Events:      []*mongoEventRecord{r},
			}

			if err := sess.DB(s.db).C("events").Insert(aggregate); err != nil {
				return ErrCouldNotSaveAggregate
			}
		} else {
			// Increment record version before inserting.
			r.Version = existing[0].Version + 1

			// Increment aggregate version on insert of new event record, and
			// only insert if version of aggregate is matching (ie not changed
			// since the query above).
			err = sess.DB(s.db).C("events").Update(
				bson.M{
					"_id":     event.AggregateID().String(),
					"version": existing[0].Version,
				},
				bson.M{
					"$push": bson.M{"events": r},
					"$inc":  bson.M{"version": 1},
				},
			)
			if err != nil {
				return ErrCouldNotSaveAggregate
			}
		}

		// Publish event on the bus.
		if s.eventBus != nil {
			s.eventBus.PublishEvent(event)
		}
	}

	return nil
}

// Load loads all events for the aggregate id from the database.
// Returns nil if no events can be found.
func (s *MongoEventStore) Load(id UUID) ([]Event, error) {
	sess := s.session.Copy()
	defer sess.Close()

	var aggregates []mongoAggregateRecord
	err := sess.DB(s.db).C("events").FindId(id.String()).Limit(1).All(&aggregates)
	if err != nil || len(aggregates) > 1 {
		return nil, ErrCouldNotLoadAggregate
	} else if len(aggregates) == 0 {
		return nil, nil
	}

	aggregate := aggregates[0]
	events := make([]Event, len(aggregate.Events))
	for i, record := range aggregate.Events {
		// Get the registered factory function for creating events.
		f, ok := s.factories[record.Type]
		if !ok {
			return nil, ErrEventNotRegistered
		}

		// Manually decode the raw BSON event.
		event := f()
		if err := record.Data.Unmarshal(event); err != nil {
			return nil, ErrCouldNotUnmarshalEvent
		}
		if events[i], ok = event.(Event); !ok {
			return nil, ErrInvalidEvent
		}

		// Set concrete event and zero out the decoded event.
		record.Event = events[i]
		record.Data = bson.Raw{}
	}

	return events, nil
}

// RegisterEventType registers an event factory for a event type. The factory is
// used to create concrete event types when loading from the database.
//
// An example would be:
//     eventStore.RegisterEventType(&MyEvent{}, func() Event { return &MyEvent{} })
func (s *MongoEventStore) RegisterEventType(event Event, factory func() Event) error {
	if _, ok := s.factories[event.EventType()]; ok {
		return ErrHandlerAlreadySet
	}

	s.factories[event.EventType()] = factory

	return nil
}

// SetDB sets the database session.
func (s *MongoEventStore) SetDB(db string) {
	s.db = db
}

// Clear clears the event storge.
func (s *MongoEventStore) Clear() error {
	if err := s.session.DB(s.db).C("events").DropCollection(); err != nil {
		return ErrCouldNotClearDB
	}
	return nil
}

// Close closes the database session.
func (s *MongoEventStore) Close() {
	s.session.Close()
}

// MongoReadRepository implements an MongoDB repository of read models.
type MongoReadRepository struct {
	session    *mgo.Session
	db         string
	collection string
	factory    func() interface{}
}

// NewMongoReadRepository creates a new MongoReadRepository.
func NewMongoReadRepository(url, database, collection string) (*MongoReadRepository, error) {
	session, err := mgo.Dial(url)
	if err != nil {
		return nil, ErrCouldNotDialDB
	}

	session.SetMode(mgo.Strong, true)
	session.SetSafe(&mgo.Safe{W: 1})

	return NewMongoReadRepositoryWithSession(session, database, collection)
}

// NewMongoReadRepositoryWithSession creates a new MongoReadRepository with a session.
func NewMongoReadRepositoryWithSession(session *mgo.Session, database, collection string) (*MongoReadRepository, error) {
	if session == nil {
		return nil, ErrNoDBSession
	}

	r := &MongoReadRepository{
		session:    session,
		db:         database,
		collection: collection,
	}

	return r, nil
}

// Save saves a read model with id to the repository.
func (r *MongoReadRepository) Save(id UUID, model interface{}) error {
	sess := r.session.Copy()
	defer sess.Close()

	if _, err := sess.DB(r.db).C(r.collection).UpsertId(id, model); err != nil {
		return ErrCouldNotSaveModel
	}
	return nil
}

// Find returns one read model with using an id. Returns
// ErrModelNotFound if no model could be found.
func (r *MongoReadRepository) Find(id UUID) (interface{}, error) {
	sess := r.session.Copy()
	defer sess.Close()

	if r.factory == nil {
		return nil, ErrModelNotSet
	}

	model := r.factory()
	err := sess.DB(r.db).C(r.collection).FindId(id).One(model)
	if err != nil {
		return nil, ErrModelNotFound
	}

	return model, nil
}

// FindCustom uses a callback to specify a custom query.
func (r *MongoReadRepository) FindCustom(callback func(*mgo.Collection) *mgo.Query) ([]interface{}, error) {
	sess := r.session.Copy()
	defer sess.Close()

	if r.factory == nil {
		return nil, ErrModelNotSet
	}

	collection := sess.DB(r.db).C(r.collection)
	query := callback(collection)

	iter := query.Iter()
	result := []interface{}{}
	model := r.factory()
	for iter.Next(model) {
		result = append(result, model)
		model = r.factory()
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}

	return result, nil
}

// PipeQuery is used for chaining query elements and creating a aggregated group query
func (r *MongoReadRepository) PipeQuery(callback func(*mgo.Collection) *mgo.Pipe) ([]interface{}, error) {
	sess := r.session.Copy()
	defer sess.Close()

	collection := sess.DB(r.db).C(r.collection)
	query := callback(collection)

	iter := query.Iter()

	var results []interface{}
	err := iter.All(&results)
	if err != nil {
		return nil, err
	}

	if err := iter.Close(); err != nil {
		return nil, err
	}

	return results, nil
}

// FindAll returns all read models in the repository.
func (r *MongoReadRepository) FindAll() ([]interface{}, error) {
	sess := r.session.Copy()
	defer sess.Close()

	if r.factory == nil {
		return nil, ErrModelNotSet
	}

	iter := sess.DB(r.db).C(r.collection).Find(nil).Iter()
	result := []interface{}{}
	model := r.factory()
	for iter.Next(model) {
		result = append(result, model)
		model = r.factory()
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}

	return result, nil
}

// Remove removes a read model with id from the repository. Returns
// ErrModelNotFound if no model could be found.
func (r *MongoReadRepository) Remove(id UUID) error {
	sess := r.session.Copy()
	defer sess.Close()

	err := sess.DB(r.db).C(r.collection).RemoveId(id)
	if err != nil {
		return ErrModelNotFound
	}

	return nil
}

// SetModel sets a factory function that creates concrete model types.
func (r *MongoReadRepository) SetModel(factory func() interface{}) {
	r.factory = factory
}

// SetDB sets the database session and database.
func (r *MongoReadRepository) SetDB(db string) {
	r.db = db
}

// Clear clears the read model database.
func (r *MongoReadRepository) Clear() error {
	if err := r.session.DB(r.db).C(r.collection).DropCollection(); err != nil {
		return ErrCouldNotClearDB
	}
	return nil
}

// Close closes a database session.
func (r *MongoReadRepository) Close() {
	r.session.Close()
}
