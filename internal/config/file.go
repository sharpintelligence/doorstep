package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	"go.yaml.in/yaml/v3"
)

const (
	EnvConfig    = "DOORSTEP_CONFIG"
	maxFileBytes = 1 << 20
)

type File struct {
	Server        FileServer     `yaml:"server"`
	Log           FileLog        `yaml:"log"`
	Database      FileDatabase   `yaml:"database"`
	GitHub        FileGitHub     `yaml:"github"`
	Registry      FileRegistry   `yaml:"registry"`
	Locks         FileLocks      `yaml:"locks"`
	Notifications []Notification `yaml:"notifications"`
	Apps          []App          `yaml:"apps"`
}

type FileServer struct {
	ListenAddr        *string   `yaml:"listen_addr"`
	MetricsListenAddr *string   `yaml:"metrics_listen_addr"`
	ShutdownDelay     *Duration `yaml:"shutdown_delay"`
	ShutdownTimeout   *Duration `yaml:"shutdown_timeout"`
	PublicURL         *string   `yaml:"public_url"`
}

type FileLog struct {
	Level  *string `yaml:"level"`
	Format *string `yaml:"format"`
}

type FileDatabase struct {
	URLFile *string `yaml:"url_file"`
}

type FileGitHub struct {
	AppID          *string `yaml:"app_id"`
	PrivateKeyFile *string `yaml:"private_key_file"`
	APIURL         *string `yaml:"api_url"`
}

type FileRegistry struct {
	CredentialsFile *string `yaml:"credentials_file"`
}

type FileLocks struct {
	ReminderInterval *Duration `yaml:"reminder_interval"`
}

func Parse(r io.Reader) (File, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxFileBytes+1))
	if err != nil {
		return File{}, err
	}
	if len(data) > maxFileBytes {
		return File{}, fmt.Errorf("file is larger than the %d byte limit", maxFileBytes)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return File{}, nil
		}
		return File{}, err
	}
	var next yaml.Node
	switch err := dec.Decode(&next); {
	case errors.Is(err, io.EOF):
		return f, nil
	case err != nil:
		return File{}, err
	default:
		return File{}, errors.New("multiple YAML documents are not supported")
	}
}

func (f File) Apply(base Config) Config {
	cfg := base
	if f.Server.ListenAddr != nil {
		cfg.Server.ListenAddr = *f.Server.ListenAddr
	}
	if f.Server.MetricsListenAddr != nil {
		cfg.Server.MetricsListenAddr = *f.Server.MetricsListenAddr
	}
	if f.Server.ShutdownDelay != nil {
		cfg.Server.ShutdownDelay = f.Server.ShutdownDelay.Std()
	}
	if f.Server.ShutdownTimeout != nil {
		cfg.Server.ShutdownTimeout = f.Server.ShutdownTimeout.Std()
	}
	if f.Server.PublicURL != nil {
		cfg.Server.PublicURL = *f.Server.PublicURL
	}
	if f.Log.Level != nil {
		cfg.Log.Level = *f.Log.Level
	}
	if f.Log.Format != nil {
		cfg.Log.Format = *f.Log.Format
	}
	if f.Database.URLFile != nil {
		cfg.Database = Database{URLFile: *f.Database.URLFile}
	}
	if f.GitHub.AppID != nil {
		cfg.GitHub.AppID = *f.GitHub.AppID
	}
	if f.GitHub.PrivateKeyFile != nil {
		cfg.GitHub.PrivateKey = ""
		cfg.GitHub.PrivateKeyFile = *f.GitHub.PrivateKeyFile
	}
	if f.GitHub.APIURL != nil {
		cfg.GitHub.APIURL = *f.GitHub.APIURL
	}
	if f.Registry.CredentialsFile != nil {
		cfg.Registry.CredentialsFile = *f.Registry.CredentialsFile
	}
	if f.Locks.ReminderInterval != nil {
		cfg.Locks.ReminderInterval = f.Locks.ReminderInterval.Std()
	}
	return cfg
}

func (f File) Catalog() Catalog {
	c := Catalog{Notifications: slices.Clone(f.Notifications)}
	for i := range c.Notifications {
		c.Notifications[i].Events = slices.Clone(c.Notifications[i].Events)
	}
	for _, a := range f.Apps {
		c.Apps = append(c.Apps, a.withDefaults())
	}
	return c
}
