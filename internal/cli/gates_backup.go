package cli

import (
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/notify"
)

func registerBackupGate(r *gates.Registry, lookupEnv LookupEnv, userAgent string, now func() time.Time) error {
	client := notify.NewHTTPClient()
	client.Timeout = gates.BackupRequestTimeout
	secrets := secretSource{lookupEnv: lookupEnv, readFile: config.ReadSecretFile}
	return gates.RegisterBackup(r, gates.BackupDeps{
		Client:     client,
		ReadSecret: func(ref config.SecretRef) (string, error) { return secrets.reader(ref)() },
		Sign:       notify.Sign,
		Now:        now,
		UserAgent:  userAgent,
	})
}
