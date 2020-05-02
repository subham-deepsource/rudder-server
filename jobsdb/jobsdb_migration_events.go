package jobsdb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
)

//MigrationEvent captures an event of export/import to recover from incase of a crash during migration
type MigrationEvent struct {
	ID            int64           `json:"ID"`
	MigrationType string          `json:"MigrationType"` //ENUM : export, import, acceptNewEvents
	FromNode      string          `json:"FromNode"`
	ToNode        string          `json:"ToNode"`
	FileLocation  string          `json:"FileLocation"`
	Status        string          `json:"Status"` //ENUM : Look up 'Values for Status'
	StartSeq      int64           `json:"StartSeq"`
	Payload       json.RawMessage `json:"Payload"`
	TimeStamp     time.Time       `json:"TimeStamp"`
}

//ENUM Values for MigrationType
const (
	ExportOp          = "export"
	ImportOp          = "import"
	AcceptNewEventsOp = "acceptNewEvents"
)

//ENUM Values for Status
const (
	SetupForExport = "setup_for_export"
	Exported       = "exported"
	Notified       = "notified"
	Completed      = "completed"

	SetupToAcceptNewEvents = "setup_to_accept_new_events"
	SetupForImport         = "setup_for_import"
	PreparedForImport      = "prepared_for_import"
	Imported               = "imported"
)

//Checkpoint writes a migration event if id is passed as 0. Else it will update status and start_sequence
func (jd *HandleT) Checkpoint(migrationEvent *MigrationEvent) int64 {
	return jd.CheckpointInTxn(nil, migrationEvent)
}

//CheckpointInTxn writes a migration event if id is passed as 0. Else it will update status and start_sequence
// If txn is passed, it will run the statement in that txn, otherwise it will execute without a transaction
func (jd *HandleT) CheckpointInTxn(txn *sql.Tx, migrationEvent *MigrationEvent) int64 {
	jd.assert(migrationEvent.MigrationType == ExportOp ||
		migrationEvent.MigrationType == ImportOp ||
		migrationEvent.MigrationType == AcceptNewEventsOp,
		fmt.Sprintf("MigrationType: %s is not a supported operation. Should be %s or %s",
			migrationEvent.MigrationType, ExportOp, ImportOp))

	var sqlStatement string
	var checkpointType string
	if migrationEvent.ID > 0 {
		sqlStatement = fmt.Sprintf(`UPDATE %s SET status = $1, start_sequence = $2 WHERE id = $3 RETURNING id`, jd.getCheckPointTableName())
		checkpointType = "update"
	} else {
		sqlStatement = fmt.Sprintf(`INSERT INTO %s (migration_type, from_node, to_node, file_location, status, start_sequence, payload, time_stamp)
									VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (file_location) DO UPDATE SET status=EXCLUDED.status RETURNING id`, jd.getCheckPointTableName())
		checkpointType = "insert"
	}

	var (
		stmt *sql.Stmt
		err  error
	)
	if txn != nil {
		stmt, err = txn.Prepare(sqlStatement)
	} else {
		stmt, err = jd.dbHandle.Prepare(sqlStatement)
	}
	jd.assertError(err)
	defer stmt.Close()

	var meID int64
	if migrationEvent.ID > 0 {
		err = stmt.QueryRow(migrationEvent.Status, migrationEvent.StartSeq, migrationEvent.ID).Scan(&meID)
	} else {
		err = stmt.QueryRow(migrationEvent.MigrationType,
			migrationEvent.FromNode,
			migrationEvent.ToNode,
			migrationEvent.FileLocation,
			migrationEvent.Status,
			migrationEvent.StartSeq,
			migrationEvent.Payload,
			time.Now()).Scan(&meID)
	}
	if txn == nil {
		jd.assertError(err)
	}
	logger.Infof("%s-Migration: %s checkpoint %s from %s to %s. file: %s, status: %s for checkpointId: %d",
		jd.tablePrefix,
		migrationEvent.MigrationType,
		checkpointType,
		migrationEvent.FromNode,
		migrationEvent.ToNode,
		migrationEvent.FileLocation,
		migrationEvent.Status,
		migrationEvent.ID)
	return meID
}

//NewSetupCheckpointEvent returns a new migration event that captures setup for export, import of new event acceptance
func NewSetupCheckpointEvent(migrationType string, node string) MigrationEvent {
	switch migrationType {
	case ExportOp:
		return NewMigrationEvent(migrationType, node, "All", SetupForExport, SetupForExport, 0)
	case AcceptNewEventsOp:
		return NewMigrationEvent(migrationType, "All", node, SetupToAcceptNewEvents, SetupToAcceptNewEvents, 0)
	case ImportOp:
		return NewMigrationEvent(migrationType, "All", node, SetupForImport, SetupForImport, 0)
	default:
		panic("Illegal usage")
	}
}

//NewMigrationEvent is a constructor for MigrationEvent struct
func NewMigrationEvent(migrationType string, fromNode string, toNode string, fileLocation string, status string, startSeq int64) MigrationEvent {
	return MigrationEvent{0, migrationType, fromNode, toNode, fileLocation, status, startSeq, []byte("{}"), time.Now()}
}

//SetupCheckpointTable creates a table
func (jd *HandleT) SetupCheckpointTable() {
	sqlStatement := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id BIGSERIAL PRIMARY KEY,
		migration_type varchar(20) NOT NULL,
		from_node varchar(64) NOT NULL,
		to_node VARCHAR(64) NOT NULL,
		file_location TEXT UNIQUE,
		status varchar(64),
		start_sequence BIGINT,
		payload JSONB,
		time_stamp TIMESTAMP NOT NULL DEFAULT NOW());`, jd.getCheckPointTableName())

	_, err := jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)
	logger.Infof("%s-Migration: %s table created", jd.GetTablePrefix(), jd.getCheckPointTableName())
}

func (jd *HandleT) getCheckPointTableName() string {
	return fmt.Sprintf("%s_%d_%d_migration_checkpoints", jd.GetTablePrefix(), misc.GetMigratingFromVersion(), misc.GetMigratingToVersion())
}

//findOrCreateDsFromSetupCheckpoint is boiler plate code for setting up for different scenarios
func (jd *HandleT) findOrCreateDsFromSetupCheckpoint(migrationType string) dataSetT {
	jd.dsListLock.Lock()
	defer jd.dsListLock.Unlock()

	dsList := jd.getDSList(true)
	setupEvent := jd.GetSetupCheckpoint(migrationType)
	if setupEvent == nil {
		me := NewSetupCheckpointEvent(migrationType, misc.GetNodeID())

		var payload dataSetT
		switch migrationType {
		case ExportOp:
			payload = jd.getLastDsForExport(dsList)
		case AcceptNewEventsOp:
			payload = jd.getDsForNewEvents(dsList)
		case ImportOp:
			payload = jd.getDsForImport(dsList)
		}

		var err error
		me.Payload, err = json.Marshal(payload)
		if err != nil {
			panic("Unable to Marshal")
		}
		//TODO: Should add a transaction around possible addNewDs above and this checkpoint
		jd.Checkpoint(&me)
		setupEvent = &me
	}
	payload := dataSetT{}
	err := json.Unmarshal(setupEvent.Payload, &payload)
	jd.assertError(err)
	return payload
}

func (jd *HandleT) getSeqNoForFileFromDB(fileLocation string, migrationType string) int64 {
	jd.assert(migrationType == ExportOp ||
		migrationType == ImportOp,
		fmt.Sprintf("MigrationType: %s is not a supported operation. Should be %s or %s",
			migrationType, ExportOp, ImportOp))

	sqlStatement := fmt.Sprintf(`SELECT start_sequence from %s WHERE file_location = $1 AND migration_type = $2 ORDER BY id DESC`, jd.getCheckPointTableName())
	stmt, err := jd.dbHandle.Prepare(sqlStatement)
	defer stmt.Close()
	jd.assertError(err)

	rows, err := stmt.Query(fileLocation, migrationType)
	defer rows.Close()
	if err != nil {
		panic("Unable to query")
	}
	rows.Next()

	var sequenceNumber int64
	sequenceNumber = 0
	err = rows.Scan(&sequenceNumber)
	if err != nil && err.Error() != "sql: Rows are closed" {
		panic("query result pares issue")
	}
	return sequenceNumber
}

//GetSetupCheckpoint gets all checkpoints and picks out the setup event for that type
func (jd *HandleT) GetSetupCheckpoint(migrationType string) *MigrationEvent {
	var setupStatus string
	switch migrationType {
	case ExportOp:
		setupStatus = SetupForExport
	case AcceptNewEventsOp:
		setupStatus = SetupToAcceptNewEvents
	case ImportOp:
		setupStatus = SetupForImport
	}
	setupEvents := jd.GetCheckpoints(migrationType, setupStatus)

	switch len(setupEvents) {
	case 0:
		return nil
	case 1:
		return setupEvents[0]
	default:
		panic("More than 1 setup event found. This should not happen")
	}

}

//GetCheckpoints gets all checkpoints and
//TODO specialize it for non setup and finish events
func (jd *HandleT) GetCheckpoints(migrationType string, status string) []*MigrationEvent {
	sqlStatement := fmt.Sprintf(`SELECT * from %s WHERE migration_type = $1 AND status = $2 ORDER BY ID ASC`, jd.getCheckPointTableName())
	stmt, err := jd.dbHandle.Prepare(sqlStatement)
	jd.assertError(err)
	defer stmt.Close()

	rows, err := stmt.Query(migrationType, status)
	if err != nil {
		panic("Unable to query")
	}
	defer rows.Close()

	migrationEvents := []*MigrationEvent{}
	for rows.Next() {
		migrationEvent := MigrationEvent{}

		err = rows.Scan(&migrationEvent.ID, &migrationEvent.MigrationType, &migrationEvent.FromNode,
			&migrationEvent.ToNode, &migrationEvent.FileLocation, &migrationEvent.Status,
			&migrationEvent.StartSeq, &migrationEvent.Payload, &migrationEvent.TimeStamp)
		if err != nil {
			panic(fmt.Sprintf("query result pares issue : %s", err.Error()))
		}
		migrationEvents = append(migrationEvents, &migrationEvent)
	}
	return migrationEvents
}

func getNumberOfJobsFromFileLocation(fileLocation string) int64 {
	slicedS := strings.FieldsFunc(fileLocation, fileLocationSplitter)
	totalJobs, _ := strconv.ParseInt(slicedS[len(slicedS)-2], 10, 64)
	return totalJobs
}

func fileLocationSplitter(r rune) bool {
	return r == '_' || r == '.'
}

func (migrationEvent *MigrationEvent) getLastJobID() int64 {
	if migrationEvent.StartSeq == 0 {
		return int64(0)
	}
	return migrationEvent.StartSeq + getNumberOfJobsFromFileLocation(migrationEvent.FileLocation) - 1
}
