package data

import (
	"encoding/json"
	"log"

	"go.etcd.io/bbolt"

	"gitlab.com/etke.cc/mrs/api/model"
	"gitlab.com/etke.cc/mrs/api/repository/batch"
)

type Data struct {
	db *bbolt.DB
	rb *batch.Batch[*model.MatrixRoom]
}

func New(path string) (*Data, error) {
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, err
	}
	err = initBuckets(db)
	if err != nil {
		return nil, err
	}

	return &Data{
		db: db,
		rb: batch.New[*model.MatrixRoom](10000, func(rooms []*model.MatrixRoom) {
			db.Update(func(tx *bbolt.Tx) error { //nolint:errcheck // checked inside
				for _, room := range rooms {
					roomb, err := json.Marshal(room)
					if err != nil {
						log.Println(room.Server, room.ID, "cannot marshal room", err)
						continue
					}

					err = tx.Bucket(roomsBucket).Put([]byte(room.ID), roomb)
					if err != nil {
						log.Println(room.Server, room.ID, "cannot add room", err)
						continue
					}
				}
				return nil
			})
		}),
	}, nil
}

// Close data repository
func (d *Data) Close() error {
	return d.db.Close()
}
