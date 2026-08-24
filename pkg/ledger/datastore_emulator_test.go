package ledger_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"cloud.google.com/go/datastore"

	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger"
	"github.com/omni-labs-jp/axios_itandi_automations/pkg/ledger/ledgertest"
)

// TestDatastoreStoreAgainstEmulator は DATASTORE_EMULATOR_HOST が設定されている
// 場合のみ、実 Datastore API に対して共通スイートを走らせる。
//
//	gcloud emulators datastore start --project=test-project --consistency=1.0
//	$(gcloud beta emulators datastore env-init)
//	go test ./pkg/ledger/...
//
// クエリの整合性が弱いとリスト系が不安定になるため --consistency=1.0 が必要。
func TestDatastoreStoreAgainstEmulator(t *testing.T) {
	if os.Getenv("DATASTORE_EMULATOR_HOST") == "" {
		t.Skip("DATASTORE_EMULATOR_HOST is not set; skipping emulator test")
	}

	ctx := context.Background()
	project := os.Getenv("DATASTORE_PROJECT_ID")
	if project == "" {
		project = "test-project"
	}
	client, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatalf("datastore client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// サブテスト間で干渉しないよう名前空間を分ける。
	var n int
	ledgertest.RunStoreTests(t, func(t *testing.T) ledger.Store {
		n++
		return ledger.NewDatastoreStoreWithClient(client, ledger.DatastoreConfig{
			Namespace: fmt.Sprintf("test-%d", n),
			Kind:      "contract_kicker_entries_test",
		})
	})
}
