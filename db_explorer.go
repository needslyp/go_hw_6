package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type dbExplorer struct {
	db         *sql.DB
	tableNames []string
	tables     map[string]tableSchema
}

type tableSchema struct {
	columnNames []string
	columns     map[string]columnSchema
	primaryKey  string
}

type columnSchema struct {
	name          string
	sqlType       string
	nullable      bool
	defaultValue  sql.NullString
	autoIncrement bool
}

func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func NewDbExplorer(db *sql.DB) (http.Handler, error) {
	explorer := &dbExplorer{
		db:         db,
		tableNames: make([]string, 0),
		tables:     make(map[string]tableSchema),
	}
	rows, err := db.Query("SHOW TABLES")
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		explorer.tableNames = append(explorer.tableNames, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	sort.Strings(explorer.tableNames)
	for _, name := range explorer.tableNames {
		schema, err := loadSchema(db, name)
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", name, err)
		}
		explorer.tables[name] = schema
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		explorer.serveHTTP(w, r)
	}
	return http.HandlerFunc(handler), nil
}

func loadSchema(db *sql.DB, name string) (tableSchema, error) {
	schema := tableSchema{columns: make(map[string]columnSchema)}
	rows, err := db.Query("SHOW FULL COLUMNS FROM " + quoteIdentifier(name))
	if err != nil {
		return schema, err
	}
	defer rows.Close()
	for rows.Next() {
		var column columnSchema
		var collation sql.NullString
		var nullable, key, extra, privileges, comment string
		if err := rows.Scan(&column.name, &column.sqlType, &collation, &nullable, &key, &column.defaultValue, &extra, &privileges, &comment); err != nil {
			return schema, err
		}
		column.nullable = nullable == "YES"
		column.autoIncrement = strings.Contains(extra, "auto_increment")
		if key == "PRI" {
			if schema.primaryKey != "" {
				return schema, fmt.Errorf("composite primary keys are not supported")
			}
			schema.primaryKey = column.name
		}
		schema.columnNames = append(schema.columnNames, column.name)
		schema.columns[column.name] = column
	}
	if err := rows.Err(); err != nil {
		return schema, err
	}
	return schema, rows.Close()
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]interface{}{"error": message})
}

func writeResponse(w http.ResponseWriter, value interface{}) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"response": value})
}

func (e *dbExplorer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		writeResponse(w, map[string]interface{}{"tables": e.tableNames})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	path = strings.TrimSuffix(path, "/")
	parts := strings.Split(path, "/")
	schema, ok := e.tables[parts[0]]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown table")
		return
	}
	if len(parts) > 2 || (len(parts) == 2 && parts[1] == "") {
		writeError(w, http.StatusNotFound, "record not found")
		return
	}
	table := quoteIdentifier(parts[0])
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			e.readRecords(w, r, table, schema, "", false)
		case http.MethodPut:
			e.createRecord(w, r, table, schema)
		default:
			methodNotAllowed(w, "GET, PUT")
		}
		return
	}
	if schema.primaryKey == "" {
		writeError(w, http.StatusInternalServerError, "table has no primary key")
		return
	}
	switch r.Method {
	case http.MethodGet:
		e.readRecords(w, r, table, schema, parts[1], true)
	case http.MethodPost:
		e.updateRecord(w, r, table, schema, parts[1])
	case http.MethodDelete:
		query := "DELETE FROM " + table
		query += " WHERE " + quoteIdentifier(schema.primaryKey) + " = ?"
		result, err := e.db.ExecContext(r.Context(), query, parts[1])
		writeAffected(w, result, err, "deleted")
	default:
		methodNotAllowed(w, "GET, POST, DELETE")
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func pagination(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func (c columnSchema) typeKind() string {
	sqlType := strings.ToLower(c.sqlType)
	words := strings.Fields(sqlType)
	base := words[0]
	bracket := strings.Index(base, "(")
	if bracket != -1 {
		base = base[:bracket]
	}
	switch base {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "year":
		return "int"
	case "float", "double", "real", "decimal", "numeric":
		return "float"
	default:
		return "string"
	}
}

func (e *dbExplorer) readRecords(w http.ResponseWriter, r *http.Request, table string, schema tableSchema, id string, single bool) {
	query := "SELECT * FROM " + table
	var args []interface{}
	if single {
		query += " WHERE " + quoteIdentifier(schema.primaryKey) + " = ? LIMIT 1"
		args = append(args, id)
	} else {
		if schema.primaryKey != "" {
			query += " ORDER BY " + quoteIdentifier(schema.primaryKey)
		}
		query += " LIMIT ? OFFSET ?"
		limit := pagination(r.URL.Query().Get("limit"), 5)
		offset := pagination(r.URL.Query().Get("offset"), 0)
		args = append(args, limit, offset)
	}
	rows, err := e.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, 500, "internal error")
		return
	}
	defer rows.Close()
	records, err := scanRecords(rows, schema)
	if err != nil {
		writeError(w, 500, "internal error")
		return
	}
	if single {
		if len(records) == 0 {
			writeError(w, 404, "record not found")
			return
		}
		writeResponse(w, map[string]interface{}{"record": records[0]})
	} else {
		writeResponse(w, map[string]interface{}{"records": records})
	}
}

func scanRecords(rows *sql.Rows, schema tableSchema) ([]map[string]interface{}, error) {
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	records := make([]map[string]interface{}, 0)
	values := make([]interface{}, len(names))
	destinations := make([]interface{}, len(names))

	for i := range values {
		destinations[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(destinations...); err != nil {
			return nil, err
		}
		record := make(map[string]interface{}, len(names))
		for i, name := range names {
			value := values[i]
			if data, ok := value.([]byte); ok {
				value = string(data)
			}
			if str, ok := value.(string); ok {
				switch schema.columns[name].typeKind() {
				case "int", "float":
					value = json.Number(str)
				}
			}
			record[name] = value
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, rows.Close()
}

func readBody(w http.ResponseWriter, r *http.Request) (map[string]interface{}, bool) {
	var data map[string]interface{}
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil || data == nil {
		writeError(w, 400, "invalid JSON")
		return nil, false
	}
	return data, true
}

func (c columnSchema) valid(value interface{}) bool {
	if value == nil {
		return c.nullable
	}
	switch c.typeKind() {
	case "int":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		if strings.Contains(strings.ToLower(c.sqlType), "unsigned") {
			_, err := strconv.ParseUint(string(number), 10, 64)
			return err == nil
		}
		_, err := number.Int64()
		return err == nil
	case "float":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := number.Float64()
		return err == nil
	default:
		_, ok := value.(string)
		return ok
	}
}

func (e *dbExplorer) createRecord(w http.ResponseWriter, r *http.Request, table string, schema tableSchema) {
	if schema.primaryKey == "" {
		writeError(w, 500, "table has no primary key")
		return
	}
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var columns []string
	var placeholders []string
	var args []interface{}
	var id interface{}
	for _, name := range schema.columnNames {
		column := schema.columns[name]
		if column.autoIncrement {
			continue
		}
		value, present := data[name]
		if !present {
			if column.defaultValue.Valid {
				continue
			}
			if column.nullable {
				value = nil
			} else if column.typeKind() == "string" {
				value = ""
			} else {
				value = json.Number("0")
			}
		}
		if !column.valid(value) {
			writeError(w, 400, "field "+name+" have invalid type")
			return
		}
		columns = append(columns, quoteIdentifier(name))
		placeholders = append(placeholders, "?")
		args = append(args, value)
		if name == schema.primaryKey {
			id = value
		}
	}
	query := "INSERT INTO " + table + " (" + strings.Join(columns, ", ") + ")"
	query += " VALUES (" + strings.Join(placeholders, ", ") + ")"
	result, err := e.db.ExecContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, 500, "internal error")
		return
	}
	if schema.columns[schema.primaryKey].autoIncrement {
		id, err = result.LastInsertId()
	}
	if err != nil {
		writeError(w, 500, "internal error")
		return
	}
	writeResponse(w, map[string]interface{}{schema.primaryKey: id})
}

func (e *dbExplorer) updateRecord(w http.ResponseWriter, r *http.Request, table string, schema tableSchema, id string) {
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	var assignments []string
	var args []interface{}
	for _, name := range schema.columnNames {
		value, present := data[name]
		if !present {
			continue
		}
		if name == schema.primaryKey || !schema.columns[name].valid(value) {
			writeError(w, 400, "field "+name+" have invalid type")
			return
		}
		assignments = append(assignments, quoteIdentifier(name)+" = ?")
		args = append(args, value)
	}
	if len(assignments) == 0 {
		writeResponse(w, map[string]interface{}{"updated": 0})
		return
	}
	args = append(args, id)
	query := "UPDATE " + table + " SET " + strings.Join(assignments, ", ")
	query += " WHERE " + quoteIdentifier(schema.primaryKey) + " = ?"
	result, err := e.db.ExecContext(r.Context(), query, args...)
	writeAffected(w, result, err, "updated")
}

func writeAffected(w http.ResponseWriter, result sql.Result, err error, key string) {
	if err != nil {
		writeError(w, 500, "internal error")
		return
	}
	count, err := result.RowsAffected()
	if err != nil {
		writeError(w, 500, "internal error")
		return
	}
	writeResponse(w, map[string]interface{}{key: count})
}
