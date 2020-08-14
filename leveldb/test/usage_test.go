package test

import (
	"fmt"
	"github.com/syndtr/goleveldb/leveldb"
	"strconv"
	"testing"
)

func TestLevleDB(t *testing.T) {
	db, err := leveldb.OpenFile("./", nil)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	for i := 1000000; i <= 2000000; i ++ {
		db.Put([]byte("key" + strconv.Itoa(i)), []byte("value" + strconv.Itoa(i)), nil)
	}
	data, err := db.Get([]byte("key1000"), nil)
	fmt.Println((string(data)))
}

