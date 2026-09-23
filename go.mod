module github.com/vodlabs-io/recorder

go 1.25.4

// The toolchain every build uses, so released binaries, container images and
// a developer's laptop are not each compiled by whatever Go they happen to
// have. CI follows this file rather than naming a version of its own.
toolchain go1.26.8

require (
	github.com/DeRuina/timberjack v1.4.2
	github.com/Eyevinn/hls-m3u8 v0.6.4
	github.com/Masterminds/semver/v3 v3.5.0
	github.com/dustin/go-humanize v1.0.1
	github.com/knadh/koanf/parsers/toml v0.1.0
	github.com/knadh/koanf/providers/cliflagv3 v1.1.1
	github.com/knadh/koanf/providers/confmap v1.0.0
	github.com/knadh/koanf/providers/file v1.2.1
	github.com/knadh/koanf/providers/posflag v1.0.1
	github.com/knadh/koanf/v2 v2.3.4
	github.com/matoous/go-nanoid/v2 v2.1.0
	github.com/nats-io/nats-server/v2 v2.14.0
	github.com/nats-io/nats.go v1.51.0
	github.com/nats-io/nkeys v0.4.15
	github.com/pelletier/go-toml/v2 v2.3.1
	github.com/sgtdi/fswatcher v1.2.0
	github.com/spf13/pflag v1.0.10
	github.com/tidwall/gjson v1.18.0
	github.com/urfave/cli/v3 v3.8.0
	github.com/zalando/go-keyring v0.2.8
	golang.org/x/sys v0.45.0
	modernc.org/sqlite v1.50.0
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/fsnotify/fsnotify v1.10.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/knadh/koanf/maps v0.1.2 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/nats-io/jwt/v2 v2.8.1 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pelletier/go-toml v1.9.5 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.org/x/tools v0.44.0 // indirect
	modernc.org/libc v1.72.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
