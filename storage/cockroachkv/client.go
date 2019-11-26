// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package cockroachkv

import (
	"bytes"
	"context"
	"database/sql"

	"github.com/cockroachdb/cockroach-go/crdb"
	"github.com/lib/pq"
	"github.com/zeebo/errs"
	monkit "gopkg.in/spacemonkeygo/monkit.v2"
	"storj.io/storj/internal/dbutil"
	"storj.io/storj/storage"
)

const (
	defaultBatchSize = 10000
	defaultBucket    = ""
)

var (
	mon = monkit.Package()
)

// Client is the entrypoint into a cockroachkv data store
type Client struct {
	URL    string
	pgConn *sql.DB
}

// New instantiates a new postgreskv client given db URL
func New(dbURL string) (*Client, error) {
	pgConn, err := sql.Open("postgres", dbURL)
	if err != nil {
		return nil, err
	}

	dbutil.Configure(pgConn, mon)

	// TODO: Need to bring this back but sourcing CockroachDB compatible schema.
	// err = schema.PrepareDB(pgConn, dbURL)
	// if err != nil {
	// 	return nil, err
	// }
	return &Client{
		URL:    dbURL,
		pgConn: pgConn,
	}, nil
}

// Close closes the client
func (client *Client) Close() error {
	return client.pgConn.Close()
}

// TODO: Need to bring this back but sourcing CockroachDB compatible schema.
// DropSchema drops the schema.
// func (client *Client) DropSchema(schema string) error {
// 	return pgutil.DropSchema(client.pgConn, schema)
// }

// Put sets the value for the provided key.
func (client *Client) Put(ctx context.Context, key storage.Key, value storage.Value) (err error) {
	defer mon.Task()(&ctx)(&err)
	return client.PutPath(ctx, storage.Key(defaultBucket), key, value)
}

// PutPath sets the value for the provided key (in the given bucket).
func (client *Client) PutPath(ctx context.Context, bucket, key storage.Key, value storage.Value) (err error) {
	defer mon.Task()(&ctx)(&err)
	if key.IsZero() {
		return storage.ErrEmptyKey.New("")
	}
	q := `
		INSERT INTO pathdata (bucket, fullpath, metadata)
			VALUES ($1:::BYTEA, $2:::BYTEA, $3:::BYTEA)
			ON CONFLICT (bucket, fullpath) DO UPDATE SET metadata = EXCLUDED.metadata
	`
	_, err = client.pgConn.Exec(q, []byte(bucket), []byte(key), []byte(value))
	return err
}

// Get looks up the provided key and returns its value (or an error).
func (client *Client) Get(ctx context.Context, key storage.Key) (_ storage.Value, err error) {
	defer mon.Task()(&ctx)(&err)
	return client.GetPath(ctx, storage.Key(defaultBucket), key)
}

// GetPath looks up the provided key (in the given bucket) and returns its value (or an error).
func (client *Client) GetPath(ctx context.Context, bucket, key storage.Key) (_ storage.Value, err error) {
	defer mon.Task()(&ctx)(&err)
	if key.IsZero() {
		return nil, storage.ErrEmptyKey.New("")
	}

	q := "SELECT metadata FROM pathdata WHERE bucket = $1:::BYTEA AND fullpath = $2:::BYTEA"
	row := client.pgConn.QueryRow(q, []byte(bucket), []byte(key))

	var val []byte
	err = row.Scan(&val)
	if err == sql.ErrNoRows {
		return nil, storage.ErrKeyNotFound.New("%q", key)
	}

	return val, Error.Wrap(err)
}

// GetAll finds all values for the provided keys (up to storage.LookupLimit).
// If more keys are provided than the maximum, an error will be returned.
func (client *Client) GetAll(ctx context.Context, keys storage.Keys) (_ storage.Values, err error) {
	defer mon.Task()(&ctx)(&err)
	return client.GetAllPath(ctx, storage.Key(defaultBucket), keys)
}

// GetAllPath finds all values for the provided keys (up to storage.LookupLimit)
// in the given bucket. if more keys are provided than the maximum, an error
// will be returned.
func (client *Client) GetAllPath(ctx context.Context, bucket storage.Key, keys storage.Keys) (_ storage.Values, err error) {
	defer mon.Task()(&ctx)(&err)
	if len(keys) > storage.LookupLimit {
		return nil, storage.ErrLimitExceeded
	}

	q := `
		SELECT metadata
		FROM pathdata pd
			RIGHT JOIN
				unnest($2:::BYTEA[]) WITH ORDINALITY pk(request, ord)
			ON (pd.fullpath = pk.request AND pd.bucket = $1:::BYTEA)
		ORDER BY pk.ord
	`
	rows, err := client.pgConn.Query(q, []byte(bucket), pq.ByteaArray(keys.ByteSlices()))
	if err != nil {
		return nil, errs.Wrap(err)
	}
	values := make([]storage.Value, 0, len(keys))
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			return nil, errs.Wrap(errs.Combine(err, rows.Close()))
		}
		values = append(values, storage.Value(value))
	}
	return values, errs.Combine(rows.Err(), rows.Close())
}

// Delete deletes the given key and its associated value.
func (client *Client) Delete(ctx context.Context, key storage.Key) (err error) {
	defer mon.Task()(&ctx)(&err)
	return client.DeletePath(ctx, storage.Key(defaultBucket), key)
}

// DeletePath deletes the given key (in the given bucket) and its associated value.
func (client *Client) DeletePath(ctx context.Context, bucket, key storage.Key) (err error) {
	defer mon.Task()(&ctx)(&err)
	if key.IsZero() {
		return storage.ErrEmptyKey.New("")
	}

	q := "DELETE FROM pathdata WHERE bucket = $1:::BYTEA AND fullpath = $2:::BYTEA"
	result, err := client.pgConn.Exec(q, []byte(bucket), []byte(key))
	if err != nil {
		return err
	}
	numRows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if numRows == 0 {
		return storage.ErrKeyNotFound.New("%q", key)
	}
	return nil
}

// List returns either a list of known keys, in order, or an error.
func (client *Client) List(ctx context.Context, first storage.Key, limit int) (_ storage.Keys, err error) {
	defer mon.Task()(&ctx)(&err)
	return storage.ListKeys(ctx, client, first, limit)
}

func (client *Client) Iterate(ctx context.Context, opts storage.IterateOptions, fn func(context.Context, storage.Iterator) error) (err error) {
	defer mon.Task()(&ctx)(&err)
	opi, err := newOrderedCockroachIterator(ctx, client, opts, defaultBatchSize)
	if err != nil {
		return err
	}
	defer func() {
		err = errs.Combine(err, opi.Close())
	}()

	return fn(ctx, opi)
}

// CompareAndSwap atomically compares and swaps oldValue with newValue
func (client *Client) CompareAndSwap(ctx context.Context, key storage.Key, oldValue, newValue storage.Value) (err error) {
	defer mon.Task()(&ctx)(&err)
	return client.CompareAndSwapPath(ctx, storage.Key(defaultBucket), key, oldValue, newValue)
}

// CompareAndSwapPath atomically compares and swaps oldValue with newValue in the given bucket
func (client *Client) CompareAndSwapPath(ctx context.Context, bucket, key storage.Key, oldValue, newValue storage.Value) (err error) {
	defer mon.Task()(&ctx)(&err)
	if key.IsZero() {
		return storage.ErrEmptyKey.New("")
	}

	if oldValue == nil && newValue == nil {
		q := "SELECT metadata FROM pathdata WHERE bucket = $1:::BYTEA AND fullpath = $2:::BYTEA"
		row := client.pgConn.QueryRow(q, []byte(bucket), []byte(key))
		var val []byte
		err = row.Scan(&val)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return Error.Wrap(err)
		}
		return storage.ErrValueChanged.New("%q", key)
	}

	if oldValue == nil {
		q := `
		INSERT INTO pathdata (bucket, fullpath, metadata) VALUES ($1:::BYTEA, $2:::BYTEA, $3:::BYTEA)
			ON CONFLICT DO NOTHING
			RETURNING 1
		`
		row := client.pgConn.QueryRow(q, []byte(bucket), []byte(key), []byte(newValue))
		var val []byte
		err = row.Scan(&val)
		if err == sql.ErrNoRows {
			return storage.ErrValueChanged.New("%q", key)
		}
		return Error.Wrap(err)
	}

	return crdb.ExecuteTx(ctx, client.pgConn, nil, func(txn *sql.Tx) error {
		q := "SELECT metadata FROM pathdata WHERE bucket = $1:::BYTEA AND fullpath = $2:::BYTEA;"
		row := txn.QueryRowContext(ctx, q, []byte(bucket), []byte(key))

		var metadata []byte
		err = row.Scan(&metadata)
		if err == sql.ErrNoRows {
			// Row not found for this bucket+fullpath combination.
			// Potentially because another concurrent transaction changed the row.
			return storage.ErrKeyNotFound.New("%q", key)
		}
		if err != nil {
			return Error.Wrap(err)
		}

		if equal := bytes.Compare(metadata, oldValue); equal != 0 {
			// If the row is found but the metadata has been already changed
			// we can't continue to delete it.
			return storage.ErrValueChanged.New("%q", key)
		}

		var res sql.Result
		if newValue == nil {
			q = `
		DELETE FROM pathdata
			WHERE pathdata.metadata = $3:::BYTEA
				AND pathdata.bucket = $1:::BYTEA
				AND pathdata.fullpath = $2:::BYTEA
		`

			res, err = txn.ExecContext(ctx, q, []byte(bucket), []byte(key), []byte(oldValue))
		} else {
			q = `
		UPDATE pathdata
			SET metadata = $4:::BYTEA
			WHERE pathdata.metadata = $3:::BYTEA
				AND pathdata.bucket = $1:::BYTEA
				AND pathdata.fullpath = $2:::BYTEA
		`
			res, err = txn.ExecContext(ctx, q, []byte(bucket), []byte(key), []byte(oldValue), []byte(newValue))
		}

		if err != nil {
			return Error.Wrap(err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return Error.Wrap(err)
		}

		if affected != 1 {
			return storage.ErrValueChanged.New("%q", key)
		}

		return nil
	})
}