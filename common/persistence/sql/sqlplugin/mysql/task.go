// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package mysql

import (
	"database/sql"
	"fmt"

	"github.com/temporalio/temporal/common/persistence/sql/sqlplugin"
)

const (
	taskQueueCreatePart = `INTO task_queues(shard_id, namespace_id, name, task_type, range_id, data, data_encoding) ` +
		`VALUES (:shard_id, :namespace_id, :name, :task_type, :range_id, :data, :data_encoding)`

	// (default range ID: initialRangeID == 1)
	createTaskQueueQry = `INSERT ` + taskQueueCreatePart

	replaceTaskQueueQry = `REPLACE ` + taskQueueCreatePart

	updateTaskQueueQry = `UPDATE task_queues SET
range_id = :range_id,
data = :data,
data_encoding = :data_encoding
WHERE
shard_id = :shard_id AND
namespace_id = :namespace_id AND
name = :name AND
task_type = :task_type
`

	listTaskQueueQry = `SELECT namespace_id, range_id, name, task_type, data, data_encoding ` +
		`FROM task_queues ` +
		`WHERE shard_id = ? AND namespace_id > ? AND name > ? AND task_type > ? ORDER BY namespace_id,name,task_type LIMIT ?`

	getTaskQueueQry = `SELECT namespace_id, range_id, name, task_type, data, data_encoding ` +
		`FROM task_queues ` +
		`WHERE shard_id = ? AND namespace_id = ? AND name = ? AND task_type = ?`

	deleteTaskQueueQry = `DELETE FROM task_queues WHERE shard_id=? AND namespace_id=? AND name=? AND task_type=? AND range_id=?`

	lockTaskQueueQry = `SELECT range_id FROM task_queues ` +
		`WHERE shard_id = ? AND namespace_id = ? AND name = ? AND task_type = ? FOR UPDATE`

	getTaskMinMaxQry = `SELECT task_id, data, data_encoding ` +
		`FROM tasks ` +
		`WHERE namespace_id = ? AND task_queue_name = ? AND task_type = ? AND task_id > ? AND task_id <= ? ` +
		` ORDER BY task_id LIMIT ?`

	getTaskMinQry = `SELECT task_id, data, data_encoding ` +
		`FROM tasks ` +
		`WHERE namespace_id = ? AND task_queue_name = ? AND task_type = ? AND task_id > ? ORDER BY task_id LIMIT ?`

	createTaskQry = `INSERT INTO ` +
		`tasks(namespace_id, task_queue_name, task_type, task_id, data, data_encoding) ` +
		`VALUES(:namespace_id, :task_queue_name, :task_type, :task_id, :data, :data_encoding)`

	deleteTaskQry = `DELETE FROM tasks ` +
		`WHERE namespace_id = ? AND task_queue_name = ? AND task_type = ? AND task_id = ?`

	rangeDeleteTaskQry = `DELETE FROM tasks ` +
		`WHERE namespace_id = ? AND task_queue_name = ? AND task_type = ? AND task_id <= ? ` +
		`ORDER BY namespace_id,task_queue_name,task_type,task_id LIMIT ?`
)

// InsertIntoTasks inserts one or more rows into tasks table
func (mdb *db) InsertIntoTasks(rows []sqlplugin.TasksRow) (sql.Result, error) {
	return mdb.conn.NamedExec(createTaskQry, rows)
}

// SelectFromTasks reads one or more rows from tasks table
func (mdb *db) SelectFromTasks(filter *sqlplugin.TasksFilter) ([]sqlplugin.TasksRow, error) {
	var err error
	var rows []sqlplugin.TasksRow
	switch {
	case filter.MaxTaskID != nil:
		err = mdb.conn.Select(&rows, getTaskMinMaxQry, filter.NamespaceID,
			filter.TaskQueueName, filter.TaskType, *filter.MinTaskID, *filter.MaxTaskID, *filter.PageSize)
	default:
		err = mdb.conn.Select(&rows, getTaskMinQry, filter.NamespaceID,
			filter.TaskQueueName, filter.TaskType, *filter.MinTaskID, *filter.PageSize)
	}
	if err != nil {
		return nil, err
	}
	return rows, err
}

// DeleteFromTasks deletes one or more rows from tasks table
func (mdb *db) DeleteFromTasks(filter *sqlplugin.TasksFilter) (sql.Result, error) {
	if filter.TaskIDLessThanEquals != nil {
		if filter.Limit == nil || *filter.Limit == 0 {
			return nil, fmt.Errorf("missing limit parameter")
		}
		return mdb.conn.Exec(rangeDeleteTaskQry,
			filter.NamespaceID, filter.TaskQueueName, filter.TaskType, *filter.TaskIDLessThanEquals, *filter.Limit)
	}
	return mdb.conn.Exec(deleteTaskQry, filter.NamespaceID, filter.TaskQueueName, filter.TaskType, *filter.TaskID)
}

// InsertIntoTaskQueues inserts one or more rows into task_queues table
func (mdb *db) InsertIntoTaskQueues(row *sqlplugin.TaskQueuesRow) (sql.Result, error) {
	return mdb.conn.NamedExec(createTaskQueueQry, row)
}

// ReplaceIntoTaskQueues replaces one or more rows in task_queues table
func (mdb *db) ReplaceIntoTaskQueues(row *sqlplugin.TaskQueuesRow) (sql.Result, error) {
	return mdb.conn.NamedExec(replaceTaskQueueQry, row)
}

// UpdateTaskQueues updates a row in task_queues table
func (mdb *db) UpdateTaskQueues(row *sqlplugin.TaskQueuesRow) (sql.Result, error) {
	return mdb.conn.NamedExec(updateTaskQueueQry, row)
}

// SelectFromTaskQueues reads one or more rows from task_queues table
func (mdb *db) SelectFromTaskQueues(filter *sqlplugin.TaskQueuesFilter) ([]sqlplugin.TaskQueuesRow, error) {
	switch {
	case filter.NamespaceID != nil && filter.Name != nil && filter.TaskType != nil:
		return mdb.selectFromTaskQueues(filter)
	case filter.NamespaceIDGreaterThan != nil && filter.NameGreaterThan != nil && filter.TaskTypeGreaterThan != nil && filter.PageSize != nil:
		return mdb.rangeSelectFromTaskQueues(filter)
	default:
		return nil, fmt.Errorf("invalid set of query filter params")
	}
}

func (mdb *db) selectFromTaskQueues(filter *sqlplugin.TaskQueuesFilter) ([]sqlplugin.TaskQueuesRow, error) {
	var err error
	var row sqlplugin.TaskQueuesRow
	err = mdb.conn.Get(&row, getTaskQueueQry, filter.ShardID, *filter.NamespaceID, *filter.Name, *filter.TaskType)
	if err != nil {
		return nil, err
	}
	return []sqlplugin.TaskQueuesRow{row}, err
}

func (mdb *db) rangeSelectFromTaskQueues(filter *sqlplugin.TaskQueuesFilter) ([]sqlplugin.TaskQueuesRow, error) {
	var err error
	var rows []sqlplugin.TaskQueuesRow
	err = mdb.conn.Select(&rows, listTaskQueueQry,
		filter.ShardID, *filter.NamespaceIDGreaterThan, *filter.NameGreaterThan, *filter.TaskTypeGreaterThan, *filter.PageSize)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].ShardID = filter.ShardID
	}
	return rows, nil
}

// DeleteFromTaskQueues deletes a row from task_queues table
func (mdb *db) DeleteFromTaskQueues(filter *sqlplugin.TaskQueuesFilter) (sql.Result, error) {
	return mdb.conn.Exec(deleteTaskQueueQry, filter.ShardID, *filter.NamespaceID, *filter.Name, *filter.TaskType, *filter.RangeID)
}

// LockTaskQueues locks a row in task_queues table
func (mdb *db) LockTaskQueues(filter *sqlplugin.TaskQueuesFilter) (int64, error) {
	var rangeID int64
	err := mdb.conn.Get(&rangeID, lockTaskQueueQry, filter.ShardID, *filter.NamespaceID, *filter.Name, *filter.TaskType)
	return rangeID, err
}
