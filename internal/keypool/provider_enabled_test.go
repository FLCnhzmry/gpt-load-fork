package keypool

import (
	"fmt"
	"testing"

	"gpt-load/internal/models"
	"gpt-load/internal/store"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newEnabledTestProvider(t *testing.T) (*KeyProvider, store.Store, *gorm.DB) {
	t.Helper()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.APIKey{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}

	st := store.NewMemoryStore()
	return NewProvider(db, st, nil, nil), st, db
}

// TestSetKeyEnabledPreservesStoreFailureCount 覆盖手动开关与并发失败计数的竞态：
// 上游自 v1.4.12 起以 store 中的 failure_count 为权威，切换 enabled 时不能把
// 已经推进的计数回写成 DB 里的旧值。
func TestSetKeyEnabledPreservesStoreFailureCount(t *testing.T) {
	p, st, db := newEnabledTestProvider(t)

	key := models.APIKey{
		KeyValue: "sk-test-enabled",
		GroupID:  1,
		Status:   models.KeyStatusActive,
		Enabled:  true,
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	hashKey := fmt.Sprintf("key:%d", key.ID)
	activeListKey := "group:1:active_keys"
	// 模拟并发失败已经把 store 计数推进到 7，而 DB 里仍是 3。
	if err := st.HSet(hashKey, map[string]any{
		"id":            fmt.Sprint(key.ID),
		"key_string":    key.KeyValue,
		"status":        models.KeyStatusActive,
		"enabled":       true,
		"failure_count": 7,
		"group_id":      "1",
		"created_at":    0,
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := st.LPush(activeListKey, key.ID); err != nil {
		t.Fatalf("seed active list: %v", err)
	}

	if _, err := p.SetKeyEnabled(key.ID, false); err != nil {
		t.Fatalf("disable key: %v", err)
	}

	details, err := st.HGetAll(hashKey)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if got := details["failure_count"]; got != "7" {
		t.Fatalf("failure_count was overwritten: want 7, got %q", got)
	}
	if got := details["enabled"]; got != "false" {
		t.Fatalf("enabled not persisted in store: got %q", got)
	}

	var stored models.APIKey
	if err := db.First(&stored, key.ID).Error; err != nil {
		t.Fatalf("read db: %v", err)
	}
	if stored.Enabled {
		t.Fatal("enabled not persisted in db")
	}

	if length, err := st.LLen(activeListKey); err != nil || length != 0 {
		t.Fatalf("disabled key still in active pool: length=%d err=%v", length, err)
	}

	// 重新启用后回到活跃池，且 store 计数仍然不被覆盖。
	if _, err := p.SetKeyEnabled(key.ID, true); err != nil {
		t.Fatalf("enable key: %v", err)
	}
	if length, err := st.LLen(activeListKey); err != nil || length != 1 {
		t.Fatalf("enabled key not back in active pool: length=%d err=%v", length, err)
	}
	if details, err = st.HGetAll(hashKey); err != nil || details["failure_count"] != "7" {
		t.Fatalf("failure_count changed after re-enable: %q err=%v", details["failure_count"], err)
	}
}

// TestSetKeyEnabledRebuildsMissingHash 保证缓存缺失时仍然写入完整的 key HASH，
// 而不是只留下一个 enabled 字段。
func TestSetKeyEnabledRebuildsMissingHash(t *testing.T) {
	p, st, db := newEnabledTestProvider(t)

	key := models.APIKey{
		KeyValue: "sk-test-missing",
		GroupID:  2,
		Status:   models.KeyStatusActive,
		Enabled:  true,
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	if _, err := p.SetKeyEnabled(key.ID, false); err != nil {
		t.Fatalf("disable key: %v", err)
	}

	details, err := st.HGetAll(fmt.Sprintf("key:%d", key.ID))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if details["id"] == "" || details["key_string"] != key.KeyValue {
		t.Fatalf("hash was not rebuilt from db: %#v", details)
	}
	if details["enabled"] != "false" {
		t.Fatalf("enabled not written: got %q", details["enabled"])
	}
}
