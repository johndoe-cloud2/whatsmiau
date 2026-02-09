package env

import (
	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

type E struct {
	Port           string `env:"PORT" envDefault:"8080"`
	DebugMode      bool   `env:"DEBUG_MODE" envDefault:"false"`
	DebugWhatsmeow bool   `env:"DEBUG_WHATSMEOW" envDefault:"false"`

	RedisURL      string `env:"REDIS_URL" envDefault:"localhost:6379"`
	RedisPassword string `env:"REDIS_PASSWORD"`
	RedisTLS      bool   `env:"REDIS_TLS" envDefault:"false"`

	ApiKey string `env:"API_KEY" envDefault:""`
	// ECS/minimal: use sqlite3 and DB_URL=file:/app/data/data.db?_foreign_keys=on (Dockerfile has /app/data)
	DBDialect string `env:"DIALECT_DB" envDefault:"sqlite3"`
	DBURL     string `env:"DB_URL" envDefault:"file:data.db?_foreign_keys=on"`

	// AWS/ECS: single webhook URL for all events from this backend; if set, overrides per-instance webhook
	WebhookURL string `env:"WEBHOOK_URL" envDefault:""`
	// URL at which this backend is reachable (for router routing); used to register in Redis backends + route:<id>
	BackendPublicURL string `env:"BACKEND_PUBLIC_URL" envDefault:""`

	GCSEnabled bool   `env:"GCS_ENABLED" envDefault:"false"`
	GCSBucket  string `env:"GCS_BUCKET" envDefault:"whatsmiau"`
	GCSURL     string `env:"GCS_URL" envDefault:"https://storage.googleapis.com"`

	// Local media: save images/audio/etc. to a folder (e.g. ./media or /app/media). MEDIA_PUBLIC_URL is the base URL for links (e.g. http://localhost:8080).
	LocalMediaPath string `env:"LOCAL_MEDIA_PATH" envDefault:""`
	MediaPublicURL string `env:"MEDIA_PUBLIC_URL" envDefault:""`

	GCL          string `json:"GCL_APP_NAME" envDefault:"whatsmiau-br-1"`
	GCLEnabled   bool   `json:"GCL_ENABLED" envDefault:"false"`
	GCLProjectID string `json:"GCL_PROJECT_ID"`

	EmitterBufferSize    int `env:"EMITTER_BUFFER_SIZE" envDefault:"2048"`
	HandlerSemaphoreSize int `env:"HANDLER_SEMAPHORE_SIZE" envDefault:"512"`

	ProxyAddresses []string `env:"PROXY_ADDRESSES" envDefault:""`      // random choices proxies ex: <SOCKS5|HTTP|HTTPS>://<username>:<password>@<host>:<port>
	ProxyStrategy  string   `env:"PROXY_STRATEGY" envDefault:"RANDOM"` // todo: implement BALANCED
	ProxyNoMedia   bool     `env:"PROXY_NO_MEDIA" envDefault:"false"`
}

var Env E

func Load() error {
	_ = godotenv.Load(".env")
	err := env.Parse(&Env)

	return err
}
