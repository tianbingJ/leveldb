package test

import (
	"fmt"
	"github.com/syndtr/goleveldb/leveldb"
	"testing"
)

func TestLevleDB(t *testing.T) {
	db, err := leveldb.OpenFile("./", nil)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	//for i := 0; i <= 1000; i ++ {
	//	db.Put([]byte("key" + strconv.Itoa(i)), []byte("value" + strconv.Itoa(i)), nil)
	//}
	data, err := db.Get([]byte("key1000"), nil)
	fmt.Println((string(data)))
}

