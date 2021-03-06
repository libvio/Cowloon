package migrator

import (
	"log"
	"strings"

	"fmt"

	"database/sql"

	"strconv"

	"time"

	"github.com/mrasu/Cowloon/pkg/db"
	"github.com/pkg/errors"
	"github.com/siddontang/go-mysql/canal"
)

const (
	copyRange     = 1000
	keyColumnName = "tenant_id"
)

type Applier struct {
	key string

	fromShard *db.ShardConnection
	toShard   *db.ShardConnection

	// TODO: Change "id" to "unique key" to support tables without "id" column
	appliedAtId    int
	maxMigrationId int
	migrated       bool
}

func NewApplier(key string, fromShard, toShard *db.ShardConnection) *Applier {
	return &Applier{
		key:         key,
		toShard:     toShard,
		fromShard:   fromShard,
		appliedAtId: 0,
		migrated:    false,
	}
}

func (a *Applier) Run() error {
	cfg := &canal.Config{
		ServerID: 1,
		Flavor:   "mysql",
		Addr:     a.fromShard.Addr,
		User:     a.fromShard.User,
	}

	c, err := canal.NewCanal(cfg)
	if err != nil {
		panic(err)
	}

	ms, err := GetMasterStatus(a.fromShard)
	if err != nil {
		panic(err)
	}

	dmlEventChan := make(chan []*db.Query)
	c.SetEventHandler(NewCanalHandler(a.fromShard.DbName, dmlEventChan, a.key))
	go c.RunFrom(ms.ToMysqlPosition())

	err = a.resetMaxMigrationId()
	if err != nil {
		return err
	}

	for {
		select {
		case queries := <-dmlEventChan:
			{
				err := a.toShard.ExecStrictQueries(queries)
				if err != nil {
					return err
				}
			}
		default:
			{
				if a.migrated {
					log.Println("do nothing...")
					time.Sleep(time.Second)
					break
				}

				if err = a.copyRows(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *Applier) resetMaxMigrationId() error {
	s := "SELECT MAX(id) FROM messages"
	columns, rows, err := a.fromShard.QueryQuery(db.NewQuery(s, []interface{}{}))
	if err != nil {
		return err
	}

	if len(rows) != 1 {
		return fmt.Errorf("resetMaxMigrationId returns %d rows", len(rows))
	}

	row := rows[0]
	if len(columns) != 1 {
		return fmt.Errorf("resetMaxMigrationId returns %d columns", len(columns))
	}

	column := row[0]
	maxId, err := a.toInt(column)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Cannot convert(%v) to int", column))
	}

	a.maxMigrationId = maxId
	return nil
}

func (a *Applier) copyRows() error {
	query := db.NewQuery("SELECT * FROM messages WHERE id > ? AND id <= ? AND "+keyColumnName+" = ? ORDER BY id LIMIT ?", []interface{}{a.appliedAtId, a.maxMigrationId, a.key, copyRange})
	columnNames, rows, err := a.fromShard.QueryQuery(query)

	if err != nil {
		return err
	}

	if len(rows) == 0 {
		a.migrated = true
		return nil
	}

	var values []string
	var args []interface{}
	var lastId int
	for _, row := range rows {
		var vs []string
		for i, column := range row {
			vs = append(vs, "?")
			args = append(args, column)

			if columnNames[i] == "id" {
				lastId, err = a.toInt(column)
				if err != nil {
					return err
				}
			}
		}

		values = append(values, "("+strings.Join(vs, ", ")+")")
	}
	insertQuery := "REPLACE INTO messages(" + strings.Join(columnNames, ",") + ") VALUES" + strings.Join(values, ", ")

	_, err = a.toShard.Exec(insertQuery, args...)
	if err != nil {
		return err
	}

	a.appliedAtId = lastId
	if len(rows) < copyRange {
		a.migrated = true
	}

	return nil
}

func (a *Applier) toInt(bytes sql.RawBytes) (int, error) {
	return strconv.Atoi(string(bytes))
}
