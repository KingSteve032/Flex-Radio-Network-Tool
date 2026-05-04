package utils

import (
	"database/sql"
	"log"
	"strings"
	"time"
)

type Users struct {
	db *sql.DB
}

const dbFile string = "flextool.db"

const create string = `
  CREATE TABLE IF NOT EXISTS users (
  id INTEGER NOT NULL PRIMARY KEY,
  username TEXT,
  client_ip TEXT UNIQUE,
  connected_time DATETIME
  );`

// UsersDb creates a sqlite database with a clients table and schema if the file does not already exist
func UsersDb() (*Users, error) {
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(create); err != nil {
		return nil, err
	}
	return &Users{db: db}, nil
}

// DeleteAllUsers removes all user records from the db
func (c *Users) DeleteAllUsers() error {
	_, err := c.db.Exec("DELETE FROM users")
	return err
}

// InsertOrKeepConnectedTime inserts a new user with connected_time=now()
// or updates username if already present (keeping the original connected_time).
func (c *Users) InsertOrKeepConnectedTime(activity VpnRouteRow) error {
	now := time.Now().UTC()
	_, err := c.db.Exec(`
		INSERT INTO users (username, client_ip, connected_time)
		VALUES (?, ?, ?)
		ON CONFLICT(client_ip) DO UPDATE SET
			username=excluded.username;
	`, activity.Name, activity.IP, now)
	return err
}

// RemoveUsersNotInList deletes users whose IPs are not in the given list
func (c *Users) RemoveUsersNotInList(activeIPs []string) error {
	if len(activeIPs) == 0 {
		_, err := c.db.Exec("DELETE FROM users")
		return err
	}
	query := "DELETE FROM users WHERE client_ip NOT IN (?" + strings.Repeat(",?", len(activeIPs)-1) + ")"
	args := make([]interface{}, len(activeIPs))
	for i, ip := range activeIPs {
		args[i] = ip
	}
	_, err := c.db.Exec(query, args...)
	return err
}

// GetUserIpAddresses returns the ip address of every user record in the db
func (c *Users) GetUserIpAddresses() ([]string, error) {
	rows, err := c.db.Query("SELECT client_ip from users")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var client_ip_list []string
	for rows.Next() {
		var client_ip string
		if err := rows.Scan(&client_ip); err != nil {
			log.Fatal(err)
		}
		client_ip_list = append(client_ip_list, client_ip)
	}
	return client_ip_list, nil
}

// GetConnectedTime retrieves the connected_time for a client
func (c *Users) GetConnectedTime(clientIP string) (time.Time, error) {
	var ts time.Time
	err := c.db.QueryRow("SELECT connected_time FROM users WHERE client_ip=?", clientIP).Scan(&ts)
	return ts, err
}

// HasIP checks if a given IP exists in the users table
func (c *Users) HasIP(ip string) (bool, error) {
	var dummy int
	err := c.db.QueryRow("SELECT 1 FROM users WHERE client_ip = ? LIMIT 1", ip).Scan(&dummy)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
