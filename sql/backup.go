// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Daniel Harrison (daniel.harrison@gmail.com)

package sql

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/internal/client"
	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/sql/parser"
	"github.com/cockroachdb/cockroach/sql/sqlbase"
	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/util/hlc"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/pkg/errors"
)

const (
	dataSSTableName      = "data.sst"
	backupDescriptorName = "BACKUP"
)

func allRangeDescriptors(txn *client.Txn) ([]roachpb.RangeDescriptor, error) {
	// TODO(dan): Iterate with some batch size.
	rows, err := txn.Scan(keys.Meta2Prefix, keys.MetaMax, 0)
	if err != nil {
		return nil, errors.Wrap(err, "unable to scan range descriptors")
	}

	rangeDescs := make([]roachpb.RangeDescriptor, len(rows))
	for i, row := range rows {
		if err := row.ValueProto(&rangeDescs[i]); err != nil {
			return nil, errors.Wrapf(err, "%s: unable to unmarshal range descriptor", row.Key)
		}
	}
	return rangeDescs, nil
}

func allSQLDescriptors(txn *client.Txn) ([]sqlbase.Descriptor, error) {
	startKey := roachpb.Key(keys.MakeTablePrefix(keys.DescriptorTableID))
	endKey := startKey.PrefixEnd()
	// TODO(dan): Iterate with some batch size.
	rows, err := txn.Scan(startKey, endKey, 0)
	if err != nil {
		return nil, errors.Wrap(err, "unable to scan SQL descriptors")
	}

	sqlDescs := make([]sqlbase.Descriptor, len(rows))
	for i, row := range rows {
		if err := row.ValueProto(&sqlDescs[i]); err != nil {
			return nil, errors.Wrapf(err, "%s: unable to unmarshal SQL descriptor", row.Key)
		}
	}
	return sqlDescs, nil
}

// Backup exports a snapshot of every kv entry into ranged sstables.
//
// The output is an sstable per range with files in the following locations:
// - /<base>/<node_id>/<key_range>/data.sst
// - <base> is given by the user and is expected to eventually be cloud storage
// - The <key_range>s are non-overlapping.
//
// TODO(dan): Bikeshed this directory structure and naming.
func Backup(
	ctx context.Context, db client.DB, base string,
) (desc sqlbase.BackupDescriptor, retErr error) {
	// TODO(dan): Optionally take a start time for an incremental backup.
	// TODO(dan): Take a uri for the path prefix and support various cloud storages.
	// TODO(dan): Figure out how permissions should work. #6713 is tracking this
	// for grpc.

	// TODO(dan): Pick an appropriate end time and set it in the txn.
	txn := client.NewTxn(ctx, db)

	rangeDescs, err := allRangeDescriptors(txn)
	if err != nil {
		return sqlbase.BackupDescriptor{}, err
	}

	sqlDescs, err := allSQLDescriptors(txn)
	if err != nil {
		return sqlbase.BackupDescriptor{}, err
	}

	var dataSize int64
	backupDescs := make([]sqlbase.BackupRangeDescriptor, len(rangeDescs))
	for i, rangeDesc := range rangeDescs {
		backupDescs[i] = sqlbase.BackupRangeDescriptor{
			StartKey:  rangeDesc.StartKey.AsRawKey(),
			EndKey:    rangeDesc.EndKey.AsRawKey(),
			StartTime: hlc.Timestamp{},
		}
		if backupDescs[i].StartKey.Compare(keys.LocalMax) < 0 {
			backupDescs[i].StartKey = keys.LocalMax
		}

		nodeID := 0
		dir := filepath.Join(base, fmt.Sprintf("%03d", nodeID))
		dir = filepath.Join(dir, fmt.Sprintf("%x-%x", rangeDesc.StartKey, rangeDesc.EndKey))
		if err := os.MkdirAll(dir, 0700); err != nil {
			return sqlbase.BackupDescriptor{}, err
		}

		// TODO(dan): Iterate with some batch size.
		kvs, err := txn.Scan(backupDescs[i].StartKey, backupDescs[i].EndKey, 0)
		if err != nil {
			return sqlbase.BackupDescriptor{}, err
		}
		if len(kvs) == 0 {
			log.Infof(ctx, "skipping backup of empty range %s-%s",
				backupDescs[i].StartKey, backupDescs[i].EndKey)
			continue
		}

		sst := engine.MakeRocksDBSstFileWriter()
		backupDescs[i].Path = filepath.Join(dir, dataSSTableName)
		if err := sst.Open(backupDescs[i].Path); err != nil {
			return sqlbase.BackupDescriptor{}, err
		}
		defer func() {
			if err := sst.Close(); err != nil && retErr == nil {
				retErr = err
			}
		}()
		// TODO(dan): Move all this iteration into cpp to avoid the cgo calls.
		for _, kv := range kvs {
			mvccKV := engine.MVCCKeyValue{
				Key:   engine.MVCCKey{Key: kv.Key, Timestamp: kv.Value.Timestamp},
				Value: kv.Value.RawBytes,
			}
			if err := sst.Add(mvccKV); err != nil {
				return sqlbase.BackupDescriptor{}, err
			}
		}
		dataSize += sst.DataSize
	}
	if err := txn.CommitOrCleanup(); err != nil {
		return sqlbase.BackupDescriptor{}, err
	}

	desc = sqlbase.BackupDescriptor{
		EndTime:  txn.Proto.MaxTimestamp,
		Ranges:   backupDescs,
		SQL:      sqlDescs,
		DataSize: dataSize,
	}

	descBuf, err := desc.Marshal()
	if err != nil {
		return sqlbase.BackupDescriptor{}, err
	}
	if err = ioutil.WriteFile(filepath.Join(base, backupDescriptorName), descBuf, 0600); err != nil {
		return sqlbase.BackupDescriptor{}, err
	}

	return desc, nil
}

// Import loads some data in sstables into the database. Only the keys between
// startKey and endKey are loaded.
func Import(
	ctx context.Context,
	sst engine.RocksDBSstFileReader,
	txn *client.Txn,
	startKey, endKey engine.MVCCKey,
) error {
	var v roachpb.Value
	importFunc := func(kv engine.MVCCKeyValue) (bool, error) {
		v = roachpb.Value{RawBytes: kv.Value}
		v.ClearChecksum()
		if log.V(3) {
			log.Infof(ctx, "Put %s %s\n", kv.Key.Key, v.PrettyPrint())
		}
		if err := txn.Put(kv.Key.Key, &v); err != nil {
			return true, err
		}
		return false, nil
	}
	return sst.Iterate(startKey, endKey, importFunc)
}

func restoreDatabase(
	ctx context.Context,
	txn *client.Txn,
	db *sqlbase.DatabaseDescriptor,
	overwrite bool,
) error {
	dbIDKey := databaseKey{db.Name}.Key()
	dbDescKey := sqlbase.MakeDescMetadataKey(db.GetID())

	existingID, err := txn.Get(dbIDKey)
	if err != nil {
		return err
	}
	if existingID.Value != nil && !overwrite {
		return errors.Errorf("database %q already exists", db.Name)
	}

	existingDesc, err := txn.Get(dbDescKey)
	if err != nil {
		return err
	}
	if existingDesc.Value != nil && !overwrite {
		return errors.Errorf("database %q already exists", db.Name)
	}

	b := &client.Batch{}
	b.Put(dbIDKey, db.ID)
	b.Put(dbDescKey, sqlbase.WrapDescriptor(db))
	return txn.Run(b)
}

func restoreTable(
	ctx context.Context,
	sst engine.RocksDBSstFileReader,
	txn *client.Txn,
	table *sqlbase.TableDescriptor,
	overwrite bool,
) error {
	log.Infof(ctx, "Restoring Table %q", table.Name)

	tableIDKey := tableKey{parentID: table.ParentID, name: table.Name}.Key()
	tableDescKey := sqlbase.MakeDescMetadataKey(table.GetID())

	tableStartKey := roachpb.Key(sqlbase.MakeIndexKeyPrefix(table, table.PrimaryIndex.ID))
	tableEndKey := tableStartKey.PrefixEnd()

	b := &client.Batch{}

	existingID, err := txn.Get(tableIDKey)
	if err != nil {
		return err
	}
	if existingID.Value != nil && !overwrite {
		return errors.Errorf("table %q already exists", table.Name)
	}
	b.Put(tableIDKey, table.ID)

	existingDesc, err := txn.Get(tableDescKey)
	if err != nil {
		return err
	}
	if existingDesc.Value != nil && !overwrite {
		return errors.Errorf("table %q already exists", table.Name)
	}
	b.Put(tableDescKey, sqlbase.WrapDescriptor(table))

	existingData, err := txn.Scan(tableStartKey, tableEndKey, 1)
	if err != nil {
		return err
	}
	if len(existingData) > 0 {
		if overwrite {
			b.DelRange(tableStartKey, tableEndKey, false)
		} else {
			return errors.Errorf("table %q already exists", table.Name)
		}
	}

	if err := txn.Run(b); err != nil {
		return err
	}

	return Import(ctx, sst, txn, engine.MVCCKey{Key: tableStartKey}, engine.MVCCKey{Key: tableEndKey})
}

func userTablesAndDBsMatchingName(
	descs []sqlbase.Descriptor, name parser.TableName,
) ([]sqlbase.Descriptor, error) {
	tableName := sqlbase.NormalizeName(name.TableName)
	dbName := sqlbase.NormalizeName(name.DatabaseName)

	matches := make([]sqlbase.Descriptor, 0, len(descs))
	dbIDsToName := make(map[sqlbase.ID]string)
	for _, desc := range descs {
		if db := desc.GetDatabase(); db != nil {
			if db.ID == keys.SystemDatabaseID {
				continue // Not a user database.
			}
			if n := sqlbase.NormalizeName(parser.Name(db.Name)); dbName == "*" || n == dbName {
				matches = append(matches, desc)
				dbIDsToName[db.ID] = n
			}
			continue
		}
	}
	for _, desc := range descs {
		if table := desc.GetTable(); table != nil {
			if _, ok := dbIDsToName[table.ParentID]; !ok {
				continue
			}
			if tableName == "*" || sqlbase.NormalizeName(parser.Name(table.Name)) == tableName {
				matches = append(matches, desc)
			}
		}
	}
	return matches, nil
}

// Restore imports a SQL table (or tables) from a set of non-overlapping sstable
// files.
func Restore(
	ctx context.Context,
	db client.DB,
	base string,
	table parser.TableName,
	overwrite bool,
) ([]sqlbase.Descriptor, error) {
	sst, err := engine.MakeRocksDBSstFileReader()
	if err != nil {
		return nil, err
	}
	defer sst.Close()

	descBytes, err := ioutil.ReadFile(filepath.Join(base, backupDescriptorName))
	if err != nil {
		return nil, err
	}
	var backupDesc sqlbase.BackupDescriptor
	if err := backupDesc.Unmarshal(descBytes); err != nil {
		return nil, err
	}
	for _, r := range backupDesc.Ranges {
		if len(r.Path) == 0 {
			continue
		}
		if err := sst.AddFile(r.Path); err != nil {
			return nil, err
		}
	}

	matches, err := userTablesAndDBsMatchingName(backupDesc.SQL, table)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, errors.Errorf("no tables found: %q", table)
	}

	// TODO(dan): This uses one giant transaction for the entire restore, which
	// works for small datasets, but not for big ones.
	return matches, db.Txn(ctx, func(txn *client.Txn) error {
		for _, desc := range matches {
			if db := desc.GetDatabase(); db != nil {
				if err := restoreDatabase(ctx, txn, db, overwrite); err != nil {
					return err
				}
				continue
			}
			table := desc.GetTable()
			if err := restoreTable(ctx, sst, txn, table, overwrite); err != nil {
				return err
			}
		}
		return nil
	})
}
