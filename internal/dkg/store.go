package dkg

import (
	bytes2 "bytes"
	"os"
	"path"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"

	pdkg "github.com/drand/drand/v2/protobuf/dkg"

	"github.com/drand/drand/v2/common/key"
	"github.com/drand/drand/v2/common/log"
)

type BoltStore struct {
	db            *bolt.DB
	log           log.Logger
	migrationLock sync.Mutex
}

const BoltFileName = "dkg.db"
const BoltStoreOpenPerm = 0660
const DirPerm = 0755

var stagedStateBucket = []byte("dkg")
var finishedStateBucket = []byte("dkg_finished")

func NewDKGStore(baseFolder string) (*BoltStore, error) {
	err := os.MkdirAll(baseFolder, DirPerm)
	if err != nil {
		return nil, err
	}
	dbPath := path.Join(baseFolder, BoltFileName)
	db, err := bolt.Open(dbPath, BoltStoreOpenPerm, nil)
	if err != nil {
		return nil, err
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(stagedStateBucket)
		if err != nil {
			return err
		}

		_, err = tx.CreateBucketIfNotExists(finishedStateBucket)
		return err
	})
	if err != nil {
		return nil, err
	}

	store := BoltStore{
		db:  db,
		log: log.New(nil, log.DebugLevel, true),
	}

	return &store, nil
}

func (s *BoltStore) GetCurrent(beaconID string) (*DBState, error) {
	dkg, err := s.get(beaconID, stagedStateBucket)

	if err != nil {
		return nil, err
	}

	if dkg == nil {
		return NewFreshState(beaconID), nil
	}
	return dkg, nil
}

func (s *BoltStore) GetFinished(beaconID string) (*DBState, error) {
	return s.get(beaconID, finishedStateBucket)
}

func (s *BoltStore) get(beaconID string, bucketName []byte) (*DBState, error) {
	var dkg *DBState

	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return errors.Errorf("%s bucket was nil - this should never happen", bucketName)
		}
		value := bucket.Get([]byte(beaconID))
		if value == nil {
			return nil
		}
		t := DBStateTOML{}
		_, err := toml.NewDecoder(bytes2.NewReader(value)).Decode(&t)
		if err != nil {
			return err
		}

		d, err := t.FromTOML()
		if err != nil {
			return err
		}
		dkg = d
		return nil
	})

	return dkg, err
}

func (s *BoltStore) SaveCurrent(beaconID string, state *DBState) error {
	return s.save(stagedStateBucket, beaconID, state)
}

func (s *BoltStore) SaveFinished(beaconID string, state *DBState) error {
	// we save to both buckets at once transactionally
	return s.db.Update(func(tx *bolt.Tx) error {
		finishedBucket := tx.Bucket(finishedStateBucket)
		currentBucket := tx.Bucket(stagedStateBucket)

		if finishedBucket == nil {
			return errors.Errorf("%s bucket was nil - this should never happen", finishedStateBucket)
		}
		if currentBucket == nil {
			return errors.Errorf("%s bucket was nil - this should never happen", stagedStateBucket)
		}

		bytesID := []byte(beaconID)
		b, err := encodeState(state)
		if err != nil {
			return err
		}

		err = finishedBucket.Put(bytesID, b)
		if err != nil {
			return err
		}
		return currentBucket.Put(bytesID, b)
	})
}
func encodeState(state *DBState) ([]byte, error) {
	var bytes []byte
	b := bytes2.NewBuffer(bytes)
	err := toml.NewEncoder(b).Encode(state.TOML())
	if err != nil {
		return nil, err
	}
	return b.Bytes(), err
}

func (s *BoltStore) save(bucketName []byte, beaconID string, state *DBState) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)

		if bucket == nil {
			return errors.Errorf("%s bucket was nil - this should never happen", bucketName)
		}

		bytesID := []byte(beaconID)
		b, err := encodeState(state)
		if err != nil {
			return err
		}

		return bucket.Put(bytesID, b)
	})
}

func (s *BoltStore) Close() error {
	if err := s.db.Close(); err != nil {
		s.log.Errorw("", "boltdb", "close", "err", err)
		return err
	}

	return nil
}

func (s *BoltStore) MigrateFromGroupfile(beaconID string, groupFile *key.Group, share *key.Share) error {
	if beaconID == "" {
		return errors.New("you must pass a beacon ID")
	}
	if groupFile == nil {
		return errors.New("you cannot migrate without passing a previous group file")
	}
	if share == nil {
		return errors.New("you cannot migrate without a previous distributed key share")
	}
	// we use a separate lock here to avoid reentrancy when calling `.SaveFinished()`
	s.migrationLock.Lock()
	defer s.migrationLock.Unlock()

	current, err := s.GetFinished(beaconID)
	if err != nil {
		return err
	}

	// if there has previously been a DKG in the database, abort!
	if current != nil {
		return errors.New("cannot migrate from groupfile if DKG state exists for beacon")
	}

	// map all the nodes from the group file into `drand.Participant`s
	participants := make([]*pdkg.Participant, len(groupFile.Nodes))

	if len(groupFile.Nodes) == 0 {
		return errors.New("you cannot migrate from a group file that doesn't contain node info")
	}
	for i, node := range groupFile.Nodes {
		pk, err := node.Key.MarshalBinary()
		if err != nil {
			return err
		}

		// MIGRATION PATH: the signature is `nil` here due to an incompatibility between v1 and v2 sigs over pub keys
		// the new signature will be filled in on first proposal using the new DKG
		participants[i] = &pdkg.Participant{
			Address:   node.Address(),
			Key:       pk,
			Signature: nil,
		}
	}

	// create an epoch 1 state with the 0th node as the leader
	state := DBState{
		BeaconID:      beaconID,
		Epoch:         1,
		State:         Complete,
		Threshold:     uint32(groupFile.Threshold),
		Timeout:       time.Now(),
		SchemeID:      groupFile.Scheme.Name,
		GenesisTime:   time.Unix(groupFile.GenesisTime, 0),
		GenesisSeed:   groupFile.GenesisSeed,
		CatchupPeriod: groupFile.CatchupPeriod,
		BeaconPeriod:  groupFile.Period,
		Leader:        participants[0],
		Remaining:     nil,
		Joining:       participants,
		Leaving:       nil,
		Acceptors:     participants,
		Rejectors:     nil,
		FinalGroup:    groupFile,
		KeyShare:      share,
	}

	return s.SaveFinished(beaconID, &state)
}

func (s *BoltStore) NukeState(beaconID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		err := tx.Bucket(stagedStateBucket).Delete([]byte(beaconID))
		if err != nil {
			return err
		}

		return tx.Bucket(finishedStateBucket).Delete([]byte(beaconID))
	})
}
