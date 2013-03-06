//MySQL driver for Go sql package
package godrv

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"github.com/ziutek/mymysql/mysql"
	_ "github.com/ziutek/mymysql/native"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

type conn struct {
	my mysql.Conn
}

type rowsRes struct {
	my mysql.Result
}

func errFilter(err error) error {
	if err == io.ErrUnexpectedEOF {
		return driver.ErrBadConn
	}
	if _, ok := err.(net.Error); ok {
		return driver.ErrBadConn
	}
	return err
}

func run(s mysql.Stmt, args []driver.Value) (rr rowsRes, err error) {
	a := (*[]interface{})(unsafe.Pointer(&args))
	rr.my, err = s.Run(*a...)
	if err != nil {
		err = errFilter(err)
	}
	return
}

func (c conn) query(query string, args []driver.Value) (rr rowsRes, err error) {
	if len(args) > 0 {
		if strings.ContainsAny(query, `'"`) {
			//err = driver.ErrSkip
			//return
			var s mysql.Stmt
			s, err = c.my.Prepare(query)
			if err != nil {
				err = errFilter(err)
				return
			}
			defer s.Delete()
			return run(s, args)
		}
		var q string
		for _, a := range args {
			i := strings.IndexRune(query, '?')
			if i == -1 {
				break
			}
			var s string
			switch v := a.(type) {
			case string:
				s = "'" + c.my.Escape(v) + "'"
			case int64:
				s = strconv.FormatInt(v, 10)
			case time.Time:
				s = v.Format(mysql.TimeFormat)
			case []byte:
				s = "'" + c.my.Escape(string(v)) + "'"
			case bool:
				if v {
					s = "1"
				} else {
					s = "0"
				}
			case float64:
				s = strconv.FormatFloat(v, 'e', 12, 64)
			default:
				s = "'" + c.my.Escape(fmt.Sprint(a)) + "'"
			}
			q += query[:i] + s
			query = query[i+1:]
		}
		query = q + query
	}
	rr.my, err = c.my.Start(query)
	if err != nil {
		errFilter(err)
	}
	return
}

func (c conn) Exec(query string, args []driver.Value) (driver.Result, error) {
	return c.query(query, args)
}

func (c conn) Query(query string, args []driver.Value) (driver.Rows, error) {
	return c.query(query, args)
}

type stmt struct {
	my mysql.Stmt
}

func (c conn) Prepare(query string) (driver.Stmt, error) {
	st, err := c.my.Prepare(query)
	if err != nil {
		return nil, errFilter(err)
	}
	return stmt{st}, nil
}

func (c conn) Close() (err error) {
	err = c.my.Close()
	c.my = nil
	if err != nil {
		err = errFilter(err)
	}
	return
}

type tx struct {
	my mysql.Transaction
}

func (c conn) Begin() (driver.Tx, error) {
	t, err := c.my.Begin()
	if err != nil {
		return nil, errFilter(err)
	}
	return tx{t}, nil
}

func (t tx) Commit() (err error) {
	err = t.my.Commit()
	if err != nil {
		err = errFilter(err)
	}
	return
}

func (t tx) Rollback() (err error) {
	err = t.my.Rollback()
	if err != nil {
		err = errFilter(err)
	}
	return
}

func (s stmt) Close() (err error) {
	err = s.my.Delete()
	s.my = nil
	if err != nil {
		err = errFilter(err)
	}
	return
}

func (s stmt) NumInput() int {
	return s.my.NumParam()
}

func (s stmt) Exec(args []driver.Value) (driver.Result, error) {
	return run(s.my, args)
}

func (s stmt) Query(args []driver.Value) (driver.Rows, error) {
	return run(s.my, args)
}

func (r rowsRes) LastInsertId() (int64, error) {
	return int64(r.my.InsertId()), nil
}

func (r rowsRes) RowsAffected() (int64, error) {
	return int64(r.my.AffectedRows()), nil
}

func (r rowsRes) Columns() []string {
	flds := r.my.Fields()
	cls := make([]string, len(flds))
	for i, f := range flds {
		cls[i] = f.Name
	}
	return cls
}

func (r rowsRes) Close() (err error) {
	err = r.my.End()
	r.my = nil
	if err != nil {
		if err == mysql.ErrReadAfterEOR {
			err = nil
		} else {
			err = errFilter(err)
		}
	}
	return
}

// DATE, DATETIME, TIMESTAMP are treated as they are in Local time zone
func (r rowsRes) Next(dest []driver.Value) (err error) {
	err = r.my.ScanRow(*(*[]interface{})(unsafe.Pointer(&dest)))
	if err != nil {
		err = errFilter(err)
	}
	return
}

type Driver struct {
	// Defaults
	proto, laddr, raddr, user, passwd, db, timeout string

	initCmds []string
}

// Open new connection. The uri need to have the following syntax:
//
//   [PROTOCOL_SPECFIIC*]DBNAME/USER/PASSWD
//
// where protocol spercific part may be empty (this means connection to
// local server using default protocol). Currently possible forms:
//
//   DBNAME/USER/PASSWD
//   unix:SOCKPATH*DBNAME/USER/PASSWD
//   unix:SOCKPATH,OPTIONS*DBNAME/USER/PASSWD
//   tcp:ADDR*DBNAME/USER/PASSWD
//   tcp:ADDR,OPTIONS*DBNAME/USER/PASSWD
//
// OPTIONS can contain comma separated list of options in form:
//   opt1=VAL1,opt2=VAL2,boolopt3,boolopt4
// Currently implemented options:
//   laddr   - local address/port (eg. 1.2.3.4:0)
//   timeout - connect timeout in format accepted by time.ParseDuration
func (d *Driver) Open(uri string) (driver.Conn, error) {
	cfg := *d // copy default configuration
	pd := strings.SplitN(uri, "*", 2)
	if len(pd) == 2 {
		// Parse protocol part of URI
		p := strings.SplitN(pd[0], ":", 2)
		if len(p) != 2 {
			return nil, errors.New("Wrong protocol part of URI")
		}
		cfg.proto = p[0]
		options := strings.Split(p[1], ",")
		cfg.raddr = options[0]
		for _, o := range options[1:] {
			kv := strings.SplitN(o, "=", 2)
			var k, v string
			if len(kv) == 2 {
				k, v = kv[0], kv[1]
			} else {
				k, v = o, "true"
			}
			switch k {
			case "laddr":
				cfg.laddr = v
			case "timeout":
				cfg.timeout = v
			default:
				return nil, errors.New("Unknown option: " + k)
			}
		}
		// Remove protocol part
		pd = pd[1:]
	}
	// Parse database part of URI
	dup := strings.SplitN(pd[0], "/", 3)
	if len(dup) != 3 {
		return nil, errors.New("Wrong database part of URI")
	}
	cfg.db = dup[0]
	cfg.user = dup[1]
	cfg.passwd = dup[2]

	// Establish the connection
	c := conn{mysql.New(
		cfg.proto, cfg.laddr, cfg.raddr, cfg.user, cfg.passwd, cfg.db,
	)}
	if cfg.timeout != "" {
		to, err := time.ParseDuration(cfg.timeout)
		if err != nil {
			return nil, err
		}
		c.my.SetTimeout(to)
	}
	for _, q := range cfg.initCmds {
		c.my.Register(q) // Register initialisation commands
	}
	if err := c.my.Connect(); err != nil {
		return nil, errFilter(err)
	}
	c.my.NarrowTypeSet(true)
	c.my.FullFieldInfo(false)
	return &c, nil
}

// Driver automatically registered in database/sql
var d = Driver{proto: "tcp", raddr: "127.0.0.1:3306"}

// Registers initialisation commands.
// This is workaround, see http://codereview.appspot.com/5706047
func Register(query string) {
	d.initCmds = append(d.initCmds, query)
}

func init() {
	Register("SET NAMES utf8")
	sql.Register("mymysql", &d)
}
