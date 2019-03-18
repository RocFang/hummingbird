package tools

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/RocFang/hummingbird/common/conf"
)

var DB_NAME = "andrewd.db"

type dbInstance struct {
	db                     *sql.DB
	serviceErrorExpiration time.Duration
	deviceErrorExpiration  time.Duration
}

func newDB(serverconf *conf.Config, memoryDBID string) (*dbInstance, error) {
	// nil serverconf indicates test mode / in memory db ; memoryDBID will be
	// used in this case to differentiate dbs, such as for independent tests.
	db := &dbInstance{}
	var err error
	if serverconf != nil {
		db.serviceErrorExpiration = time.Duration(serverconf.GetInt("andrewd", "service_error_expiration", 3600)) * time.Second
		db.deviceErrorExpiration = time.Duration(serverconf.GetInt("andrewd", "device_error_expiration", 3600)) * time.Second
		sqlDir, ok := serverconf.Get("andrewd", "sql_dir")
		if !ok {
			sqlDir = serverconf.GetDefault("drive_watch", "sql_dir", "/var/local/hummingbird")
		}
		err = os.MkdirAll(sqlDir, 0755)
		if err != nil {
			return nil, err
		}
		db.db, err = sql.Open("sqlite3", filepath.Join(sqlDir, DB_NAME)+"?psow=1&_txlock=immediate&mode=rw")
		if err != nil {
			return nil, err
		}
	} else {
		db.serviceErrorExpiration = 3600 * time.Second
		db.deviceErrorExpiration = 3600 * time.Second
		if memoryDBID == "" {
			db.db, err = sql.Open("sqlite3", "file::memory:?cache=shared")
		} else {
			db.db, err = sql.Open("sqlite3", "file:"+memoryDBID+"?mode=memory&cache=shared")
		}
		if err != nil {
			return nil, err
		}
	}
	db.db.SetMaxOpenConns(1)
	_, err = db.db.Exec(`
        PRAGMA synchronous = NORMAL;
        PRAGMA cache_size = -4096;
        PRAGMA temp_store = MEMORY;
        PRAGMA journal_mode = WAL;
        PRAGMA busy_timeout = 25000;

        CREATE TABLE IF NOT EXISTS replication_queue (
            create_date TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            update_date TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            rtype TEXT NOT NULL,          -- account, container, object
            policy INTEGER NOT NULL,      -- only used with object
            partition INTEGER NOT NULL,   -- the partition number to replicate
            reason TEXT NOT NULL,         -- ring, dispersion, quarantine
            from_device INTEGER NOT NULL, -- device id in ring to replicate from, < 0 = any
            to_device INTEGER NOT NULL    -- device id in ring to replicate to, must be valid device
        );

        CREATE INDEX IF NOT EXISTS ix_replication_queue_rtype_policy_update_date ON replication_queue (rtype, policy, update_date);

        CREATE TABLE IF NOT EXISTS dispersion_scan_failure (
            create_date TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            rtype TEXT NOT NULL,        -- account, container, object
            policy INTEGER NOT NULL,    -- only used with object
            partition INTEGER NOT NULL, -- the partition number to replicate
            service TEXT NOT NULL,      -- ip:port of service erroring, or...
            device INTEGER NOT NULL     -- ...device id in ring of device erroring
        );

        CREATE TABLE IF NOT EXISTS process_pass (
            process TEXT NOT NULL,                      -- dispersion populate, dispersion scan, quarantine repair, ...
            rtype TEXT NOT NULL,                        -- account, container, object
            policy INTEGER NOT NULL,                    -- only used with object
            start_date TIMESTAMP DEFAULT 0,             -- when the process last started, 0 = never ran
            progress_date TIMESTAMP DEFAULT 0,          -- when the progress was last updated, 0 = never updated
            progress TEXT,                              -- depends on the process
            complete_date TIMESTAMP DEFAULT 0,          -- when the process completed, 0 = is running or never ran
            previous_progress TEXT NOT NULL DEFAULT "", -- last progress from previous run, depends on the process
            previous_complete_date TIMESTAMP DEFAULT 0  -- when the process previously completed, 0 = is running or never ran
        );

        CREATE TABLE IF NOT EXISTS ring_hash (
            create_date TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            update_date TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            rtype TEXT NOT NULL,                -- account, container, object
            policy INTEGER NOT NULL,            -- only used with object
            hash TEXT NOT NULL,                 -- MD5 of on-disk file
            next_rebalance TIMESTAMP            -- Next scheduled rebalance attempt or NULL/IsZero if stable
        );

        -- records multiple states of each server, keeping a configurable time
        -- range of entries, letting us detect how long a server has been down
        -- and if it's been "bouncing" for a while, etc.
        CREATE TABLE IF NOT EXISTS server_state (
            ip TEXT NOT NULL,
            port INTEGER NOT NULL,
            recorded TIMESTAMP NOT NULL,
            state INTEGER NOT NULL      -- 0 = down, 1 = up
        );

        CREATE INDEX IF NOT EXISTS ix_server_state_ip_port_recorded ON server_state (ip, port, recorded);

        -- similar to server_state
        CREATE TABLE IF NOT EXISTS device_state (
            ip TEXT NOT NULL,
            port INTEGER NOT NULL,
            device TEXT NOT NULL,
            recorded TIMESTAMP NOT NULL,
            state INTEGER NOT NULL,     -- 0 = unmounted, 1 = mounted
            size INTEGER,
            used INTEGER
        );

        CREATE INDEX IF NOT EXISTS ix_device_state_ip_port_recorded ON device_state (ip, port, recorded);

        CREATE TABLE IF NOT EXISTS ring_log (
            create_date TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            rtype TEXT NOT NULL,                -- account, container, object
            policy INTEGER NOT NULL,            -- only used with object
            reason TEXT NOT NULL
        );

        CREATE INDEX IF NOT EXISTS ix_ring_log_rtype_policy_create_date ON ring_log (rtype, policy, create_date);
    `)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func (db *dbInstance) queuePartitionReplication(typ string, policy int, partition uint64, reason string, fromDeviceID, toDeviceID int) error {
	var tx *sql.Tx
	var rows *sql.Rows
	var err error
	defer func() {
		if rows != nil {
			rows.Close()
		}
		if tx != nil {
			tx.Rollback()
		}
	}()
	tx, err = db.db.Begin()
	if err != nil {
		return err
	}
	rows, err = tx.Query(`
        SELECT 1 FROM replication_queue
        WHERE rtype = ?
          AND policy = ?
          AND partition = ?
          AND reason = ?
          AND from_device = ?
          AND to_device = ?
    `, typ, policy, partition, reason, fromDeviceID, toDeviceID)
	if err != nil {
		return err
	}
	if rows.Next() { // entry already
		return nil
	}
	rows.Close()
	rows = nil
	_, err = tx.Exec(`
        INSERT INTO replication_queue
        (rtype, policy, partition, reason, from_device, to_device)
        VALUES (?, ?, ?, ?, ?, ?)
    `, typ, policy, partition, reason, fromDeviceID, toDeviceID)
	if err != nil {
		return err
	}
	err = tx.Commit()
	if err != nil {
		return err
	}
	tx = nil
	return nil
}

type queuedReplication struct {
	created      time.Time
	updated      time.Time
	typ          string
	policy       int
	partition    int
	reason       string
	fromDeviceID int
	toDeviceID   int
}

// queuedReplications returns the queued replications for the ring type
// (account, container, object), policy index, and reason. Entries will be
// sorted by oldest queued to newest. You can set typ == "" for all types,
// policy < 0 for all policies, and reason == "" for all reasons.
func (db *dbInstance) queuedReplications(typ string, policy int, reason string) ([]*queuedReplication, error) {
	var qrs []*queuedReplication
	var rows *sql.Rows
	var err error
	defer func() {
		if rows != nil {
			rows.Close()
		}
	}()
	query := `
        SELECT create_date, update_date, rtype, policy, partition, reason, from_device, to_device
        FROM replication_queue
    `
	var wheres []string
	var args []interface{}
	if typ != "" {
		wheres = append(wheres, "rtype = ?")
		args = append(args, typ)
	}
	if policy >= 0 {
		wheres = append(wheres, "policy = ?")
		args = append(args, policy)
	}
	if reason != "" {
		wheres = append(wheres, "reason = ?")
		args = append(args, reason)
	}
	if len(wheres) > 0 {
		query += " WHERE " + wheres[0]
		wheres = wheres[1:]
	}
	for _, where := range wheres {
		query += " AND " + where
	}
	query += " ORDER BY update_date"
	rows, err = db.db.Query(query, args...)
	if err != nil {
		return qrs, err
	}
	for rows.Next() {
		qr := &queuedReplication{}
		if err = rows.Scan(&qr.created, &qr.updated, &qr.typ, &qr.policy, &qr.partition, &qr.reason, &qr.fromDeviceID, &qr.toDeviceID); err != nil {
			return qrs, err
		}
		qrs = append(qrs, qr)
	}
	return qrs, nil
}

// updateQueuedReplication will update the qr.updated field for this queue
// replication, so that it will be placed at the back of the queue for future
// retries.
func (db *dbInstance) updateQueuedReplication(qr *queuedReplication) error {
	now := time.Now()
	_, err := db.db.Exec(`
        UPDATE replication_queue
        SET update_date = ?
        WHERE rtype = ?
          AND policy = ?
          AND partition = ?
          AND reason = ?
          AND from_device = ?
          AND to_device = ?
    `, now, qr.typ, qr.policy, qr.partition, qr.reason, qr.fromDeviceID, qr.toDeviceID)
	if err != nil {
		return err
	}
	qr.updated = now
	return err
}

func (db *dbInstance) clearQueuedReplication(qr *queuedReplication) error {
	_, err := db.db.Exec(`
        DELETE FROM replication_queue
        WHERE rtype = ?
          AND policy = ?
          AND partition = ?
          AND reason = ?
          AND from_device = ?
          AND to_device = ?
    `, qr.typ, qr.policy, qr.partition, qr.reason, qr.fromDeviceID, qr.toDeviceID)
	return err
}

func (db *dbInstance) clearDispersionScanFailures(typ string, policy int) error {
	_, err := db.db.Exec(`
        DELETE FROM dispersion_scan_failure
        WHERE rtype = ? AND policy = ?
    `, typ, policy)
	return err
}

func (db *dbInstance) recordDispersionScanFailure(typ string, policy int, partition uint64, service string, deviceID int) error {
	_, err := db.db.Exec(`
        INSERT INTO dispersion_scan_failure
        (rtype, policy, partition, service, device)
        VALUES (?, ?, ?, ?, ?)
    `, typ, policy, partition, service, deviceID)
	return err
}

type dispersionScanFailure struct {
	time      time.Time
	partition int
	service   string
	deviceID  int
}

func (db *dbInstance) dispersionScanFailures(typ string, policy int) ([]*dispersionScanFailure, error) {
	var dsfs []*dispersionScanFailure
	var rows *sql.Rows
	var err error
	defer func() {
		if rows != nil {
			rows.Close()
		}
	}()
	rows, err = db.db.Query(`
        SELECT create_date, partition, service, device
        FROM dispersion_scan_failure
        WHERE rtype = ?
          AND policy = ?
        ORDER BY create_date
    `, typ, policy)
	if err != nil {
		return dsfs, err
	}
	for rows.Next() {
		dsf := &dispersionScanFailure{}
		if err = rows.Scan(&dsf.time, &dsf.partition, &dsf.service, &dsf.deviceID); err != nil {
			return dsfs, err
		}
		dsfs = append(dsfs, dsf)
	}
	return dsfs, nil
}

func (db *dbInstance) startProcessPass(process, typ string, policy int) error {
	var tx *sql.Tx
	var rows *sql.Rows
	var err error
	defer func() {
		if rows != nil {
			rows.Close()
		}
		if tx != nil {
			tx.Rollback()
		}
	}()
	tx, err = db.db.Begin()
	if err != nil {
		return err
	}
	rows, err = tx.Query(`
        SELECT progress, complete_date FROM process_pass
        WHERE process = ?
          AND rtype = ?
          AND policy = ?
    `, process, typ, policy)
	if err != nil {
		return err
	}
	if rows.Next() { // entry already
		var previousProgress string
		var previousCompleteDate time.Time
		rows.Scan(&previousProgress, &previousCompleteDate)
		rows.Close()
		rows = nil
		if previousProgress != "" {
			_, err = tx.Exec(`
                UPDATE process_pass
                SET start_date = ?,
                    progress_date = 0,
                    progress = "",
                    complete_date = 0,
                    previous_progress = ?,
                    previous_complete_date = ?
                WHERE process = ?
                  AND rtype = ?
                  AND policy = ?
            `, time.Now(), previousProgress, previousCompleteDate, process, typ, policy)
		} else {
			_, err = tx.Exec(`
                UPDATE process_pass
                SET start_date = ?,
                    progress_date = 0,
                    progress = "",
                    complete_date = 0
                WHERE process = ?
                  AND rtype = ?
                  AND policy = ?
            `, time.Now(), process, typ, policy)
		}
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		tx = nil
		return nil
	}
	rows.Close()
	rows = nil
	if _, err = tx.Exec(`
        INSERT INTO process_pass
        (process, rtype, policy, start_date, progress_date, progress, complete_date)
        VALUES (?, ?, ?, ?, 0, "", 0)
    `, process, typ, policy, time.Now()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	tx = nil
	return nil
}

func (db *dbInstance) progressProcessPass(process, typ string, policy int, progress string) error {
	_, err := db.db.Exec(`
        UPDATE process_pass
        SET progress_date = ?,
            progress = ?
        WHERE process = ?
          AND rtype = ?
          AND policy = ?
    `, time.Now(), progress, process, typ, policy)
	return err
}

func (db *dbInstance) completeProcessPass(process, typ string, policy int) error {
	_, err := db.db.Exec(`
        UPDATE process_pass
        SET complete_date = ?
        WHERE process = ?
          AND rtype = ?
          AND policy = ?
    `, time.Now(), process, typ, policy)
	return err
}

// processPass returns start_date, progress_date, progress, and complete_date.
func (db *dbInstance) processPass(process, typ string, policy int) (time.Time, time.Time, string, time.Time, error) {
	var rows *sql.Rows
	var err error
	var start time.Time
	var progress time.Time
	var progressText string
	var complete time.Time
	defer func() {
		if rows != nil {
			rows.Close()
		}
	}()
	if rows, err = db.db.Query(`
        SELECT start_date, progress_date, progress, complete_date
        FROM process_pass
        WHERE process = ?
          AND rtype = ?
          AND policy = ?
    `, process, typ, policy); err != nil {
		return start, progress, progressText, complete, err
	}
	if rows.Next() {
		err = rows.Scan(&start, &progress, &progressText, &complete)
	}
	if start.UnixNano() == 0 {
		start = time.Time{}
	}
	if complete.UnixNano() == 0 {
		complete = time.Time{}
	}
	return start, progress, progressText, complete, err
}

type processPassData struct {
	process              string
	rtype                string
	policy               int
	startDate            time.Time
	progressDate         time.Time
	progress             string
	completeDate         time.Time
	previousProgress     string
	previousCompleteDate time.Time
}

func (db *dbInstance) processPasses() ([]*processPassData, error) {
	var rows *sql.Rows
	var err error
	var data []*processPassData
	defer func() {
		if rows != nil {
			rows.Close()
		}
	}()
	if rows, err = db.db.Query(`
        SELECT process, rtype, policy, start_date, progress_date, progress, complete_date, previous_progress, previous_complete_date
        FROM process_pass
    `); err != nil {
		return data, err
	}
	for rows.Next() {
		ppd := &processPassData{}
		if err = rows.Scan(&ppd.process, &ppd.rtype, &ppd.policy, &ppd.startDate, &ppd.progressDate, &ppd.progress, &ppd.completeDate, &ppd.previousProgress, &ppd.previousCompleteDate); err != nil {
			return data, err
		}
		if ppd.startDate.UnixNano() == 0 {
			ppd.startDate = time.Time{}
		}
		if ppd.progressDate.UnixNano() == 0 {
			ppd.progressDate = time.Time{}
		}
		if ppd.completeDate.UnixNano() == 0 {
			ppd.completeDate = time.Time{}
		}
		if ppd.previousCompleteDate.UnixNano() == 0 {
			ppd.previousCompleteDate = time.Time{}
		}
		data = append(data, ppd)
	}
	return data, nil
}

func (db *dbInstance) setRingHash(typ string, policy int, hsh string, nextRebalance time.Time) error {
	var tx *sql.Tx
	var rows *sql.Rows
	var err error
	defer func() {
		if rows != nil {
			rows.Close()
		}
		if tx != nil {
			tx.Rollback()
		}
	}()
	tx, err = db.db.Begin()
	if err != nil {
		return err
	}
	rows, err = tx.Query(`
        SELECT 1 FROM ring_hash
        WHERE rtype = ?
          AND policy = ?
    `, typ, policy)
	if err != nil {
		return err
	}
	if rows.Next() { // entry already
		rows.Close()
		rows = nil
		if _, err = tx.Exec(`
            UPDATE ring_hash
            SET update_date = ?, hash = ?, next_rebalance = ?
            WHERE rtype = ?
              AND policy = ?
        `, time.Now(), hsh, nextRebalance, typ, policy); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		tx = nil
		return nil
	}
	rows.Close()
	rows = nil
	if _, err = tx.Exec(`
        INSERT INTO ring_hash
        (update_date, rtype, policy, hash, next_rebalance)
        VALUES (?, ?, ?, ?, ?)
    `, time.Now(), typ, policy, hsh, nextRebalance); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	tx = nil
	return nil
}

func (db *dbInstance) ringHash(typ string, policy int) (string, time.Time, error) {
	var rows *sql.Rows
	var err error
	var hsh string
	var nextRebalance time.Time
	defer func() {
		if rows != nil {
			rows.Close()
		}
	}()
	if rows, err = db.db.Query(`
        SELECT hash, next_rebalance
        FROM ring_hash
        WHERE rtype = ?
          AND policy = ?
    `, typ, policy); err != nil {
		return hsh, nextRebalance, err
	}
	if rows.Next() {
		err = rows.Scan(&hsh, &nextRebalance)
	}
	if nextRebalance.UnixNano() == 0 {
		nextRebalance = time.Time{}
	}
	return hsh, nextRebalance, err
}

type stateEntry struct {
	recorded time.Time
	state    bool
	size     int64
	used     int64
}

func (db *dbInstance) serverStates(ip string, port int) ([]*stateEntry, error) {
	var rows *sql.Rows
	var states []*stateEntry
	var err error
	defer func() {
		if rows != nil {
			rows.Close()
		}
	}()
	if rows, err = db.db.Query(`
        SELECT recorded, state
        FROM server_state
        WHERE ip = ?
          AND port = ?
        ORDER BY recorded DESC
    `, ip, port); err != nil {
		return states, err
	}
	for rows.Next() {
		var recorded time.Time
		var state int
		if err = rows.Scan(&recorded, &state); err != nil {
			return states, err
		}
		states = append(states, &stateEntry{recorded: recorded, state: state == 1})
	}
	err = rows.Err()
	return states, err
}

func (db *dbInstance) addServerState(ip string, port int, up bool, retention time.Time) error {
	state := 0
	if up {
		state = 1
	}
	_, err := db.db.Exec(`
        INSERT INTO server_state
        (ip, port, recorded, state)
        VALUES (?, ?, ?, ?)
    `, ip, port, time.Now(), state)
	if err != nil {
		return err
	}
	_, err = db.db.Exec(`
        DELETE FROM server_state
        WHERE recorded < ?
    `, retention)
	return err
}

func (db *dbInstance) deviceStates(ip string, port int, device string) ([]*stateEntry, error) {
	var rows *sql.Rows
	var states []*stateEntry
	var err error
	defer func() {
		if rows != nil {
			rows.Close()
		}
	}()
	if rows, err = db.db.Query(`
        SELECT recorded, state, size, used
        FROM device_state
        WHERE ip = ?
          AND port = ?
          AND device = ?
        ORDER BY recorded DESC
    `, ip, port, device); err != nil {
		return states, err
	}
	for rows.Next() {
		var recorded time.Time
		var state int
		var size int64
		var used int64
		if err = rows.Scan(&recorded, &state, &size, &used); err != nil {
			return states, err
		}
		states = append(states, &stateEntry{recorded: recorded, state: state == 1, size: size, used: used})
	}
	err = rows.Err()
	return states, err
}

func (db *dbInstance) addDeviceState(ip string, port int, device string, mounted bool, retention time.Time, size, used int64) error {
	state := 0
	if mounted {
		state = 1
	}
	_, err := db.db.Exec(`
        INSERT INTO device_state
        (ip, port, device, recorded, state, size, used)
        VALUES (?, ?, ?, ?, ?, ?, ?)
    `, ip, port, device, time.Now(), state, size, used)
	if err != nil {
		return err
	}
	_, err = db.db.Exec(`
        DELETE FROM device_state
        WHERE recorded < ?
    `, retention)
	return err
}

type ringLogEntry struct {
	Time   time.Time
	Reason string
}

func (db *dbInstance) ringLogs(typ string, policy int) ([]*ringLogEntry, error) {
	rows, err := db.db.Query(`
        SELECT create_date, reason
        FROM ring_log
        WHERE rtype = ? AND policy = ?
        ORDER BY create_date
    `, typ, policy)
	if rows != nil {
		defer rows.Close()
	}
	if err != nil {
		return nil, err
	}
	var entries []*ringLogEntry
	for rows.Next() {
		var t time.Time
		var r string
		if err = rows.Scan(&t, &r); err != nil {
			return entries, err
		}
		entries = append(entries, &ringLogEntry{Time: t, Reason: r})
	}
	err = rows.Err()
	return entries, err
}

func (db *dbInstance) addRingLog(typ string, policy int, reason string) error {
	_, err := db.db.Exec(`
        INSERT INTO ring_log
        (rtype, policy, reason)
        VALUES (?, ?, ?)
    `, typ, policy, reason)
	return err
}
