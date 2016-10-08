package main

import (
	"bytes"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

var (
	db *sqlx.DB

	supportedDbTypes       = []string{"pg", "mysql"}
	supportedOutputFormats = []string{"c", "o"}

	dbTypeToDriverMap = map[string]string{
		"pg":    "postgres",
		"mysql": "mysql",
	}

	dbDefaultPorts = map[string]string{
		"pg":    "5432",
		"mysql": "3306",
	}

	// command line args
	help           bool
	verbose        bool
	dbType         string
	user           string
	pswd           string
	dbName         string
	schema         string
	host           string
	port           string
	outputFilePath string
	outputFormat   string
	packageName    string
	prefix         string
	suffix         string

	isMastermindStructable         bool
	isMastermindStructableOnly     bool
	isMastermindStructableRecorder bool
)

type Table struct {
	TableName string `db:"table_name"`
	Columns   []Column
}

type Column struct {
	OrdinalPosition        int            `db:"ordinal_position"`
	ColumnName             string         `db:"column_name"`
	DataType               string         `db:"data_type"`
	ColumnDefault          sql.NullString `db:"column_default"`
	IsNullable             string         `db:"is_nullable"`
	CharacterMaximumLength sql.NullInt64  `db:"character_maximum_length"`
	NumericPrecision       sql.NullInt64  `db:"numeric_precision"`
	ColumnKey              string         `db:"column_key"` // mysql specific
	Extra                  string         `db:"extra"`      // mysql specific
}

// TODO refactor without code duplications
type Database interface {
	GetTables() (tables []*Table, err error)
	PrepareGetColumnsOfTableStmt() (err error)
	GetColumnsOfTable(table *Table) (err error)
}

type PostgreDatabase struct {
	GetColumnsOfTableStmt *sqlx.Stmt
}

func (pg *PostgreDatabase) GetTables() (tables []*Table, err error) {

	err = db.Select(&tables, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_type = 'BASE TABLE'
		AND table_schema = $1
		ORDER BY table_name
	`, schema)

	if verbose {
		if err != nil {
			fmt.Println("> Error at GetTables()")
			fmt.Printf("> schema: %q\r\n", schema)
		}
	}

	return tables, err
}

func (pg *PostgreDatabase) PrepareGetColumnsOfTableStmt() (err error) {

	pg.GetColumnsOfTableStmt, err = db.Preparex(`
		SELECT
		  ordinal_position,
		  column_name,
		  data_type,
		  column_default,
		  is_nullable,
		  character_maximum_length,
		  numeric_precision
		FROM information_schema.columns
		WHERE table_name = $1
		AND table_schema = $2
		ORDER BY ordinal_position
	`)

	return err
}

func (pg *PostgreDatabase) GetColumnsOfTable(table *Table) (err error) {

	pg.GetColumnsOfTableStmt.Select(&table.Columns, table.TableName, schema)

	if verbose {
		if err != nil {
			fmt.Printf("> Error at GetColumnsOfTable(%v)\r\n", table.TableName)
			fmt.Printf("> schema: %q\r\n", schema)
		}
	}

	return err
}

type MySQLDatabase struct {
	GetColumnsOfTableStmt *sqlx.Stmt
}

func (mysql *MySQLDatabase) GetTables() (tables []*Table, err error) {

	err = db.Select(&tables, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_type = 'BASE TABLE'
		AND table_schema = ?
		ORDER BY table_name
	`, dbName)

	if verbose {
		if err != nil {
			fmt.Println("> Error at GetTables()")
			fmt.Printf("> schema: %q\r\n", dbName)
		}
	}

	return tables, err
}

func (mysql *MySQLDatabase) PrepareGetColumnsOfTableStmt() (err error) {

	mysql.GetColumnsOfTableStmt, err = db.Preparex(`
		SELECT
		  ordinal_position,
		  column_name,
		  data_type,
		  column_default,
		  is_nullable,
		  character_maximum_length,
		  numeric_precision,
		  column_key,
		  extra
		FROM information_schema.columns
		WHERE table_name = ?
		AND table_schema = ?
		ORDER BY ordinal_position
	`)

	return err
}

func (mysql *MySQLDatabase) GetColumnsOfTable(table *Table) (err error) {

	mysql.GetColumnsOfTableStmt.Select(&table.Columns, table.TableName, dbName)

	if verbose {
		if err != nil {
			fmt.Printf("> Error at GetColumnsOfTable(%v)\r\n", table.TableName)
			fmt.Printf("> schema: %q\r\n", schema)
		}
	}

	return err
}

func main() {

	prepareCmdArgs()

	if help {
		flag.Usage()
		return
	}

	err := handleCmdArgs()
	if err != nil {
		fmt.Println(err)
		return
	}

	err = connect()
	if err != nil {
		fmt.Println(err)
		return
	}
	defer db.Close()

	var database Database

	switch dbType {
	case "mysql":
		database = new(MySQLDatabase)
	default: // pg
		database = new(PostgreDatabase)
	}

	err = run(database)

	if err != nil {
		fmt.Println(err)
	}
}

func prepareCmdArgs() {
	flag.BoolVar(&help, "?", false, "shows help and usage")
	flag.BoolVar(&help, "help", false, "shows help and usage")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.StringVar(&dbType, "t", "pg", fmt.Sprintf("type of database to use, currently supported: %v", supportedDbTypes))
	flag.StringVar(&user, "u", "postgres", "user to connect to the database")
	flag.StringVar(&pswd, "p", "", "password of user")
	flag.StringVar(&dbName, "d", "postgres", "database name")
	flag.StringVar(&schema, "s", "public", "schema name")
	flag.StringVar(&host, "h", "127.0.0.1", "host of database")
	flag.StringVar(&port, "port", "", "port of database host, if not specified, it will be the default ports for the supported databases")

	flag.StringVar(&outputFilePath, "of", "./output", "output file path")
	flag.StringVar(&outputFormat, "format", "c", "camelCase (c) or original (o)")
	flag.StringVar(&prefix, "pre", "", "prefix for file- and struct names")
	flag.StringVar(&suffix, "suf", "", "suffix for file- and struct names")
	flag.StringVar(&packageName, "pn", "dto", "package name")

	flag.BoolVar(&isMastermindStructable, "st", false, "generate struct for use in Masterminds/structable (https://github.com/Masterminds/structable)")
	flag.BoolVar(&isMastermindStructableOnly, "sto", false, "generate struct ONLY for use in Masterminds/structable (https://github.com/Masterminds/structable)")
	flag.BoolVar(&isMastermindStructableRecorder, "str", false, "generate a structable.Recorder (requires -st or -sto flag)")

	flag.Parse()
}

func handleCmdArgs() (err error) {

	if !stringInSlice(dbType, supportedDbTypes) {
		return errors.New(fmt.Sprintf("type of database %q not supported! %v", dbType, supportedDbTypes))
	}

	if !stringInSlice(outputFormat, supportedOutputFormats) {
		return errors.New(fmt.Sprintf("output format %q not supported! %v", outputFormat, supportedOutputFormats))
	}

	if err = verifyOutputPath(); err != nil {
		return err
	}

	if port == "" {
		port = dbDefaultPorts[dbType]
	}

	if packageName == "" {
		return errors.New("name of package can not be empty!")
	}

	return err
}

func verifyOutputPath() (err error) {

	info, err := os.Stat(outputFilePath)

	if os.IsNotExist(err) {
		return errors.New(fmt.Sprintf("output file path %q does not exists!", outputFilePath))
	}

	if !info.Mode().IsDir() {
		return errors.New(fmt.Sprintf("output file path %q is not a directory!", outputFilePath))
	}

	outputFilePath, err = filepath.Abs(outputFilePath + "/")

	return err
}

func prepareDataSourceName() (dataSourceName string) {
	switch dbType {
	case "mysql":
		dataSourceName = fmt.Sprintf("%v:%v@tcp(%v:%v)/%v", user, pswd, host, port, dbName)
	default: // pg
		dataSourceName = fmt.Sprintf("host=%v port=%v user=%v dbname=%v password=%v sslmode=disable", host, port, user, dbName, pswd)
	}
	return dataSourceName
}

func connect() (err error) {
	db, err = sqlx.Connect(dbTypeToDriverMap[dbType], prepareDataSourceName())
	if err != nil {
		usingPswd := "no"
		if pswd != "" {
			usingPswd = "yes"
		}
		return errors.New(fmt.Sprintf("Connection to Database (type=%q, user=%q, database=%q, host='%v:%v' (using password: %v) failed!", dbType, user, dbName, host, port, usingPswd))
	}
	return db.Ping()
}

func run(db Database) (err error) {

	fmt.Printf("running for %q...\r\n", dbType)

	tables, err := db.GetTables()

	if err != nil {
		return err
	}

	if verbose {
		fmt.Printf("> count of tables: %v\r\n", len(tables))
	}

	err = db.PrepareGetColumnsOfTableStmt()

	if err != nil {
		return err
	}

	for _, table := range tables {

		if verbose {
			fmt.Printf("> processing table %q\r\n", table.TableName)
		}

		err = db.GetColumnsOfTable(table)

		if err != nil {
			return err
		}

		err = createStructOfTable(table)

		if err != nil {
			if verbose {
				fmt.Printf(">Error at createStructOfTable(%v)\r\n", table.TableName)
			}
			return err
		}
	}

	fmt.Println("done!")

	return err
}

// TODO refactor to clean code
func createStructOfTable(table *Table) (err error) {

	var buffer, colBuffer bytes.Buffer
	var isNullable bool
	timeIndicator := 0
	mastermindStructableAnnotation := ""

	for _, column := range table.Columns {

		colName := strings.Title(column.ColumnName)
		if outputFormat == "c" {
			colName = camelCaseString(colName)
		}
		colType, isTime := mapDbColumnTypeToGoType(column.DataType, column.IsNullable)

		if isMastermindStructable || isMastermindStructableOnly {

			isPk := ""
			if strings.Contains(column.ColumnDefault.String, "nextval") || // pg
				(strings.Contains(column.ColumnKey, "PRI") && strings.Contains(column.Extra, "auto_increment")) { //mysql
				isPk = `,PRIMARY_KEY,SERIAL,AUTO_INCREMENT`
			}

			mastermindStructableAnnotation = ` stbl:"` + column.ColumnName + isPk + `"`
		}

		if isMastermindStructableOnly {
			colBuffer.WriteString("\t" + colName + " " + colType + " `" + mastermindStructableAnnotation + "`\n")
		} else {
			colBuffer.WriteString("\t" + colName + " " + colType + " `db:\"" + column.ColumnName + "\"" + mastermindStructableAnnotation + "`\n")
		}

		// collect some info for later use
		if column.IsNullable == "YES" {
			isNullable = true
		}
		if isTime {
			timeIndicator++
		}
	}

	if isMastermindStructableRecorder && (isMastermindStructable || isMastermindStructableOnly) {
		colBuffer.WriteString("\t\nstructable.Recorder\n")
	}

	// create file
	tableName := strings.Title(prefix + table.TableName + suffix)
	if outputFormat == "c" {
		tableName = camelCaseString(tableName)
	}
	fileName := tableName + ".go"
	outFile, err := os.Create(outputFilePath + fileName)

	if err != nil {
		return err
	}

	// write head infos
	buffer.WriteString("package " + packageName + "\n\n")

	// do imports
	if isNullable || timeIndicator > 0 || isMastermindStructable || isMastermindStructableOnly {
		buffer.WriteString("import (\n")

		if isNullable {
			buffer.WriteString("\t\"database/sql\"\n")
		}

		if timeIndicator > 0 {
			if isNullable {
				buffer.WriteString("\t\n\"github.com/lib/pq\"\n")
			} else {
				buffer.WriteString("\t\"time\"\n")
			}
		}

		if isMastermindStructableRecorder && (isMastermindStructable || isMastermindStructableOnly) {
			buffer.WriteString("\t\n\"github.com/Masterminds/structable\"\n")
		}

		buffer.WriteString(")\n\n")
	}

	// write struct with fields
	buffer.WriteString("type " + tableName + " struct {\n")
	buffer.WriteString(colBuffer.String())
	buffer.WriteString("}")

	// format it
	formatedFile, _ := format.Source(buffer.Bytes())

	// and save it in file
	outFile.Write(formatedFile)
	outFile.Sync()
	outFile.Close()

	return err
}

func mapDbColumnTypeToGoType(dbDataType string, isNullable string) (goType string, isTime bool) {

	isTime = false

	// first row: postgresql datatypes  // TODO bitstrings, enum, other special types
	// second row: additional mysql datatypes not covered by first row // TODO bit, enums, set
	// and so on

	switch dbDataType {
	case "integer", "bigint", "bigserial", "smallint", "smallserial", "serial",
		"int", "tinyint", "mediumint":
		goType = "int"
		if isNullable == "YES" {
			goType = "sql.NullInt64"
		}
	case "double precision", "numeric", "decimal", "real",
		"float", "double":
		goType = "float64"
		if isNullable == "YES" {
			goType = "sql.NullFloat64"
		}
	case "character varying", "character", "text",
		"char", "varchar", "binary", "varbinary", "blob":
		goType = "string"
		if isNullable == "YES" {
			goType = "sql.NullString"
		}
	case "time", "timestamp", "time with time zone", "timestamp with time zone", "time without time zone", "timestamp without time zone",
		"date", "datetime", "year":
		goType = "time.Time"
		if isNullable == "YES" {
			goType = "pq.NullTime"
		}
		isTime = true
	case "boolean":
		goType = "bool"
		if isNullable == "YES" {
			goType = "sql.NullBool"
		}
	default:
		goType = "sql.NullString"
	}

	return goType, isTime
}

func camelCaseString(s string) (cc string) {
	splitted := strings.Split(s, "_")

	if len(splitted) == 1 {
		return strings.Title(s)
	}

	for _, part := range splitted {
		cc += strings.Title(strings.ToLower(part))
	}
	return cc
}

func stringInSlice(needle string, haystack []string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}