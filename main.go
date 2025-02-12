package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/eventstore/lmdb"
	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/fiatjaf/khatru"
	"github.com/fiatjaf/khatru/blossom"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
	"github.com/spf13/afero"
)

type Config struct {
	RelayName        string
	RelayPubkey      string
	RelayDescription string
	DBEngine         *string
	DBPath           *string
	PostgresUser     *string
	PostgresPassword *string
	PostgresDB       *string
	PostgresHost     *string
	PostgresPort     *string
	TeamDomain       string
	BlossomEnabled   bool
	BlossomPath      *string
	BlossomURL       *string
}

type NostrData struct {
	Names  map[string]string   `json:"names"`
	Relays map[string][]string `json:"relays"`
}

var data NostrData
var relay *khatru.Relay
var db DBBackend
var fs afero.Fs
var config Config

func main() {
	relay = khatru.NewRelay()
	config := LoadConfig()

	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)

	fetchNostrData(config.TeamDomain)

	go func() {
		for {
			time.Sleep(1 * time.Hour)
			fetchNostrData(config.TeamDomain)
		}
	}()

	relay.RejectEvent = append(relay.RejectEvent, func(ctx context.Context, event *nostr.Event) (reject bool, msg string) {
		for _, pubkey := range data.Names {
			if event.PubKey == pubkey {
				return false, "" // allow
			}
		}
		return true, "you're not part of the team"
	})

	if !config.BlossomEnabled {
		fmt.Println("running on :3334")
		http.ListenAndServe(":3334", relay)
		return
	}

	bl := blossom.New(relay, *config.BlossomURL)
	bl.Store = blossom.EventStoreBlobIndexWrapper{Store: db, ServiceURL: bl.ServiceURL}
	bl.StoreBlob = append(bl.StoreBlob, func(ctx context.Context, sha256 string, body []byte) error {

		file, err := fs.Create(*config.BlossomPath + sha256)
		if err != nil {
			return err
		}
		if _, err := io.Copy(file, bytes.NewReader(body)); err != nil {
			return err
		}
		return nil
	})

	bl.LoadBlob = append(bl.LoadBlob, func(ctx context.Context, sha256 string) (io.ReadSeeker, error) {
		return fs.Open(*config.BlossomPath + sha256)
	})
	bl.DeleteBlob = append(bl.DeleteBlob, func(ctx context.Context, sha256 string) error {
		return fs.Remove(*config.BlossomPath + sha256)
	})
	bl.RejectUpload = append(bl.RejectUpload, func(ctx context.Context, event *nostr.Event, size int, ext string) (bool, string, int) {
		for _, pubkey := range data.Names {
			if pubkey == event.PubKey {
				return false, ext, size
			}
		}

		return true, "you're not part of the team'", 403
	})

	fmt.Println("running on :3334")
	http.ListenAndServe(":3334", relay)
}

func fetchNostrData(teamDomain string) {
	response, err := http.Get("https://" + teamDomain + "/.well-known/nostr.json")
	if err != nil {
		log.Printf("Error getting well known file: %v", err)
		return
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		log.Printf("Error reading response body: %v", err)
		return
	}

	var newData NostrData
	err = json.Unmarshal(body, &newData)
	if err != nil {
		log.Printf("Error unmarshalling JSON: %v", err)
		return
	}

	data = newData
	for pubkey, names := range data.Names {
		fmt.Println(pubkey, names)
	}

	log.Println("Updated NostrData from .well-known file")
}

func LoadConfig() Config {
	err := godotenv.Load(".env")
	if err != nil {
		log.Fatalf("Error loading .env file")
	}

	config = Config{
		RelayName:        getEnv("RELAY_NAME"),
		RelayPubkey:      getEnv("RELAY_PUBKEY"),
		RelayDescription: getEnv("RELAY_DESCRIPTION"),
		DBEngine:         getEnvNullable("DB_ENGINE"),
		DBPath:           getEnvNullable("DB_PATH"),
		PostgresUser:     getEnvNullable("POSTGRES_USER"),
		PostgresPassword: getEnvNullable("POSTGRES_PASSWORD"),
		PostgresDB:       getEnvNullable("POSTGRES_DB"),
		PostgresHost:     getEnvNullable("POSTGRES_HOST"),
		PostgresPort:     getEnvNullable("POSTGRES_PORT"),
		TeamDomain:       getEnv("TEAM_DOMAIN"),
		BlossomEnabled:   getEnvBool("BLOSSOM_ENABLED"),
		BlossomPath:      getEnvNullable("BLOSSOM_PATH"),
		BlossomURL:       getEnvNullable("BLOSSOM_URL"),
	}

	relay.Info.Name = config.RelayName
	relay.Info.PubKey = config.RelayPubkey
	relay.Info.Description = config.RelayDescription
	if config.DBPath == nil {
		defaultPath := "db/"
		config.DBPath = &defaultPath
	}

	db = newDBBackend(*config.DBPath)

	if err := db.Init(); err != nil {
		panic(err)
	}

	fs = afero.NewOsFs()
	if config.BlossomEnabled {
		if config.BlossomPath == nil {
			log.Fatalf("Blossom enabled but no path set")
		}
		fs.MkdirAll(*config.BlossomPath, 0755)
	}

	return config
}

func getEnv(key string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		log.Fatalf("Environment variable %s not set", key)
	}
	return value
}

func getEnvBool(key string) bool {
	value, exists := os.LookupEnv(key)
	if !exists {
		return false
	}
	return value == "true"
}

func getEnvNullable(key string) *string {
	value, exists := os.LookupEnv(key)
	if !exists {
		return nil
	}
	return &value
}

type DBBackend interface {
	Init() error
	Close()
	CountEvents(ctx context.Context, filter nostr.Filter) (int64, error)
	DeleteEvent(ctx context.Context, evt *nostr.Event) error
	QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error)
	SaveEvent(ctx context.Context, evt *nostr.Event) error
	ReplaceEvent(ctx context.Context, evt *nostr.Event) error
}

func newDBBackend(path string) DBBackend {
	if config.DBEngine == nil {
		defaultEngine := "postgres"
		config.DBEngine = &defaultEngine
	}

	switch *config.DBEngine {
	case "lmdb":
		return newLMDBBackend(path)
	case "badger":
		return &badger.BadgerBackend{
			Path: path,
		}
	default:
		return newPostgresBackend()
	}
}

func newLMDBBackend(path string) *lmdb.LMDBBackend {
	return &lmdb.LMDBBackend{
		Path: path,
	}
}

func newPostgresBackend() DBBackend {
	return &postgresql.PostgresBackend{
		DatabaseURL: fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
			*config.PostgresUser, *config.PostgresPassword, *config.PostgresHost, *config.PostgresPort, *config.PostgresDB),
	}
}
