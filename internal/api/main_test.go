package api

import (
	"testing"
	"time"

	"github.com/mdayat/go-template/internal/testdb"
)

func TestMain(m *testing.M) {
	// pgx returns times in the local zone. A zone other than UTC makes a reply time that wasn't
	// turned into UTC fail a test on every machine, CI (which runs on UTC) included.
	time.Local = time.FixedZone("UTC+7", 7*60*60)
	testdb.Main(m)
}
