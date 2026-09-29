// тут лежит тестовый код
// менять вам может потребоваться только коннект к базе
package main

import (
	"database/sql"
	"fmt"
	"net/http"

	_ "github.com/go-sql-driver/mysql"
)

// Локальная учебная БД из docker-compose.yaml. DSN также используют тесты.
const DSN = "db_explorer:dev_password@tcp(127.0.0.1:3306)/db_explorer?charset=utf8mb4&timeout=5s&readTimeout=5s&writeTimeout=5s"

func main() {
	db, err := sql.Open("mysql", DSN)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	err = db.Ping() // вот тут будет первое подключение к базе
	if err != nil {
		panic(err)
	}

	handler, err := NewDbExplorer(db)

	if err != nil {
		panic(err)
	}

	fmt.Println("starting server at :8082")
	http.ListenAndServe(":8082", handler)
}
