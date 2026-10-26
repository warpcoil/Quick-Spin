package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const dbUser, dbPass = "quickspin", "quickspin"

// dbFlavor is "mariadb" or "mysql", decided by the server binary.
func (a *App) dbFlavor() string {
	a.flavorOnce.Do(func() { a.flavor = a.detectDBFlavor() })
	return a.flavor
}

func (a *App) detectDBFlavor() string {
	srv := a.dbTool("mariadbd", "mysqld")
	if strings.HasPrefix(filepath.Base(srv), "mariadbd") {
		return "mariadb"
	}
	if srv == "" {
		return "mariadb"
	}
	out, _ := exec.Command(srv, "--version").Output()
	if bytes.Contains(bytes.ToLower(out), []byte("mariadb")) {
		return "mariadb"
	}
	return "mysql"
}

func (a *App) dbName() string {
	if a.dbFlavor() == "mysql" {
		return "MySQL"
	}
	return "MariaDB"
}

// dbBaseDir is the install root of a downloaded server (holds share/, lib/ ...).
func dbBaseDir(server string) string {
	return filepath.Dir(filepath.Dir(server))
}

func (a *App) startMariaDB() error {
	server := a.dbTool("mariadbd", "mysqld")
	if server == "" {
		hint := "it is downloaded automatically on Windows and macOS"
		if isLinux {
			hint = "install MariaDB with your package manager (e.g. apt install mariadb-server), or set paths.mariadb in config.json"
		}
		return fmt.Errorf("no MariaDB/MySQL server found — %s", hint)
	}
	flavor := a.dbFlavor()
	name := a.dbName()
	datadir := a.data("mysql")
	if !exists(filepath.Join(datadir, "mysql")) {
		if err := a.initDatabase(server, flavor, datadir); err != nil {
			return err
		}
	}
	initSQL := a.data("tmp", "mysql-init.sql")
	sql := ""
	auth := "IDENTIFIED BY"
	if flavor == "mysql" {
		auth = "IDENTIFIED WITH mysql_native_password BY" // PHP connects without TLS
	}
	for _, host := range []string{"localhost", "127.0.0.1", "%"} {
		sql += fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%s' %s '%s';\nGRANT ALL PRIVILEGES ON *.* TO '%s'@'%s' WITH GRANT OPTION;\n", dbUser, host, auth, dbPass, dbUser, host)
	}
	if err := os.WriteFile(initSQL, []byte(sql), 0o644); err != nil {
		return err
	}

	os.Remove(filepath.Join(datadir, "mysqld.pid")) // stale after a crash or power loss
	inRuntime := strings.HasPrefix(server, a.Runtime+string(os.PathSeparator))
	s := &Service{
		Name: "mariadb", AutoRestart: true, InJob: true, Hidden: true,
		Log: NewLogBuf(a.data("logs", "mariadb.log"), 300),
		Build: func() (*exec.Cmd, error) {
			args := []string{
				"--no-defaults",
				"--datadir=" + datadir,
				"--port=" + strconv.Itoa(a.cfg.Ports.MariaDB),
				"--bind-address=127.0.0.1",
				"--skip-name-resolve",
				"--init-file=" + initSQL,
				"--character-set-server=utf8mb4",
				"--collation-server=utf8mb4_unicode_ci",
				"--sql-mode=NO_ENGINE_SUBSTITUTION",
				"--max-allowed-packet=1G",
				"--innodb-buffer-pool-size=256M",
				"--innodb-flush-log-at-trx-commit=2",
				"--pid-file=" + filepath.Join(datadir, "mysqld.pid"),
			}
			if inRuntime {
				args = append(args, "--basedir="+dbBaseDir(server))
			}
			if flavor == "mysql" {
				args = append(args, "--mysql-native-password=ON", "--mysqlx=OFF")
			}
			if isWindows {
				args = append(args, "--console")
			} else {
				args = append(args, "--socket="+filepath.Join(datadir, "mysqld.sock"))
				if os.Geteuid() == 0 {
					args = append(args, "--user=root")
				}
			}
			return exec.Command(server, args...), nil
		},
		StopFn: func(cmd *exec.Cmd) {
			if admin := a.dbTool("mariadb-admin", "mysqladmin"); admin != "" {
				c := exec.Command(admin, "--no-defaults", "-h", "127.0.0.1", "-P", strconv.Itoa(a.cfg.Ports.MariaDB), "-u", dbUser, "shutdown")
				c.Env = append(os.Environ(), "MYSQL_PWD="+dbPass)
				hideCmd(c)
				if c.Run() == nil {
					return
				}
			}
			gracefulStop(cmd.Process.Pid)
		},
	}
	a.addService(s)
	s.Start()
	a.logf("Starting %s on 127.0.0.1:%d…", name, a.cfg.Ports.MariaDB)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := a.sqlQuery("", "SELECT 1"); err == nil {
			return nil
		}
		if st := s.Status(); st.State == "failed" {
			return fmt.Errorf("%s failed to start: %s — see %s", name, st.Error, a.data("logs", "mariadb.log"))
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become ready — see %s", name, a.data("logs", "mariadb.log"))
}

func (a *App) initDatabase(server, flavor, datadir string) error {
	a.logf("Initialising the database (first run)…")
	os.RemoveAll(datadir)
	inRuntime := strings.HasPrefix(server, a.Runtime+string(os.PathSeparator))
	var c *exec.Cmd
	if flavor == "mysql" {
		args := []string{"--no-defaults", "--initialize-insecure", "--datadir=" + datadir}
		if inRuntime {
			args = append(args, "--basedir="+dbBaseDir(server))
		}
		if !isWindows && os.Geteuid() == 0 {
			args = append(args, "--user=root")
		}
		c = exec.Command(server, args...)
	} else {
		tool := a.dbTool("mariadb-install-db", "mysql_install_db")
		if tool == "" {
			return fmt.Errorf("mariadb-install-db not found")
		}
		var args []string
		if isWindows {
			args = []string{"--datadir=" + datadir}
		} else {
			if err := os.MkdirAll(datadir, 0o755); err != nil {
				return err
			}
			args = []string{"--no-defaults", "--datadir=" + datadir, "--auth-root-authentication-method=normal", "--skip-test-db"}
			switch {
			case inRuntime:
				args = append(args, "--basedir="+dbBaseDir(server))
			case strings.HasPrefix(server, "/usr/sbin"):
				args = append(args, "--basedir=/usr")
			case strings.Contains(server, "/homebrew/") || strings.HasPrefix(server, "/usr/local/"):
				args = append(args, "--basedir="+dbBaseDir(server))
			}
			if os.Geteuid() == 0 {
				args = append(args, "--user=root")
			}
		}
		c = exec.Command(tool, args...)
	}
	hideCmd(c)
	out, err := c.CombinedOutput()
	os.WriteFile(a.data("logs", "mariadb-install.log"), out, 0o644)
	if err != nil {
		os.RemoveAll(datadir)
		return fmt.Errorf("database initialisation failed: %v — see %s", err, a.data("logs", "mariadb-install.log"))
	}
	return nil
}

// mysqlCmd builds a client command connected to our server.
func (a *App) mysqlCmd(db string, extra ...string) (*exec.Cmd, error) {
	client := a.dbTool("mariadb", "mysql")
	if client == "" {
		return nil, fmt.Errorf("database client not found")
	}
	args := []string{"--no-defaults", "-h", "127.0.0.1", "-P", strconv.Itoa(a.cfg.Ports.MariaDB), "-u", dbUser, "--default-character-set=utf8mb4"}
	args = append(args, extra...)
	if db != "" {
		args = append(args, db)
	}
	c := exec.Command(client, args...)
	c.Env = append(os.Environ(), "MYSQL_PWD="+dbPass)
	hideCmd(c)
	return c, nil
}

// sqlQuery runs a statement and returns tab-separated rows.
func (a *App) sqlQuery(db, q string) ([][]string, error) {
	c, err := a.mysqlCmd(db, "-N", "-B", "-e", q)
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\r\n"), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows, nil
}
