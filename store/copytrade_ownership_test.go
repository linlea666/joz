package store

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func ownershipDatabase(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path+"?_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sql, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sql.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sql.Close() })
	return db
}
func TestAccountOwnershipAtomicAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owners.db")
	db1 := ownershipDatabase(t, path)
	if err := db1.AutoMigrate(&Trader{}, &CopyTradeContext{}, &CopyTradeOwnership{}, &CopyTradeAccountFence{}, &CopyTradeOrder{}, &CopyTradeAction{}); err != nil {
		t.Fatal(err)
	}
	if err := db1.Create(&Trader{ID: "trader", UserID: "user", Name: "test", ExchangeID: "account"}).Error; err != nil {
		t.Fatal(err)
	}
	db2 := ownershipDatabase(t, path)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successful atomic.Int32
	for i, db := range []*gorm.DB{db1, db2} {
		wg.Add(1)
		go func(i int, db *gorm.DB) {
			defer wg.Done()
			<-start
			id := []string{"first", "second"}[i]
			if err := NewCopyTradeStore(db).CreateContext(&CopyTradeContext{ID: id, TraderID: "trader", ExchangeID: "account", Symbol: "BTCUSDT", Direction: "LONG", State: "NEW"}); err == nil {
				successful.Add(1)
			}
		}(i, db)
	}
	close(start)
	wg.Wait()
	if successful.Load() != 1 {
		t.Fatalf("wanted exactly one account owner, got %d", successful.Load())
	}
	var n int64
	if err := db1.Model(&CopyTradeContext{}).Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("atomic plan failed n=%d err=%v", n, err)
	}
}

func TestActiveAccountCredentialsCannotChange(t *testing.T) {
	db := ownershipDatabase(t, filepath.Join(t.TempDir(), "credentials.db"))
	if err := db.AutoMigrate(&Trader{}, &Exchange{}, &CopyTradeContext{}, &CopyTradeOwnership{}, &CopyTradeAccountFence{}, &CopyTradeOrder{}, &CopyTradeAction{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Trader{ID: "trader", UserID: "user", Name: "test", ExchangeID: "account"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Exchange{ID: "account", UserID: "user", ExchangeType: "binance", APIKey: "old-key"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := NewCopyTradeStore(db).CreateContext(&CopyTradeContext{ID: "active", TraderID: "trader", ExchangeID: "account", Symbol: "BTCUSDT", Direction: "LONG", State: "OPEN"}); err != nil {
		t.Fatal(err)
	}
	err := NewExchangeStore(db).Update("user", "account", true, "new-key", "", "", false, "", false, false, "", "", "", "", "", "", 0)
	if err == nil {
		t.Fatal("active account credentials replaced")
	}
	var after Exchange
	if err = db.First(&after, "id = ?", "account").Error; err != nil {
		t.Fatal(err)
	}
	if string(after.APIKey) != "old-key" {
		t.Fatal("credentials changed despite rejected update")
	}
}
