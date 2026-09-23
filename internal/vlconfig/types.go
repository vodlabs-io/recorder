package vlconfig

type AppConfig struct {
	Logger     globalLoggerConfig `koanf:"logger"`
	Daemon     DaemonConfig       `koanf:"daemon"`
	Session    SessionConfig      `koanf:"session"`
	Processing ProcessingConfig   `koanf:"processing"`
	Cloud      CloudConfig        `koanf:"cloud"`
	Scraper    ScraperConfig      `koanf:"scraper"`
}

type streamersConfig struct {
	URLs []string `koanf:"urls"`
}

// streamerRule maps a group of streamers to per-streamer options.
// At least one of Usernames or URLs must be non-empty.
type streamerRule struct {
	// usernames by platform name (e.g. "tiktok" -> ["a","b"]).
	usernames map[string][]string
	uRLs      []string
	RuleOptions
}

// RuleOptions holds the option overrides set by a rule. Pointer fields
// distinguish "not specified" from "explicitly zero", so later rules
// override earlier ones only for the fields they set.
//
// To add a new option:
//  1. Add a pointer field here.
//  2. Add a line in (RuleOptions).applyTo  — merge into streamerOptions.
//  3. Add a line in (RuleOptions).writeTo  — append to the ordered TOML pairs.
//  4. Add a line in (*RuleOptions).readFrom — decode from a koanf node.
//  5. Add its key to ruleOptionKeys.
//  6. Add the corresponding field on streamerOptions. Use a concrete type
//     when a fixed default applies (Monitor) and an empty value can stand in
//     for "not set" (Variant); use a pointer when the override is a bool whose
//     default comes from global config (DeletePartFiles).
type RuleOptions struct {
	Monitor *bool
	// Record, when false, blocks recording for the matched streamers and
	// cancels any in-progress recording. They stay monitored, so their live
	// status keeps updating; only recording is switched off. Distinct from
	// streamer.StatusPaused, which is the streamer pausing their own live.
	// Unset means inherit, and the effective default is true.
	Record *bool
	// Variant overrides the global [session] variant for the matched
	// streamers. Empty string is treated as "not set".
	Variant *string
	// OutputTemplate overrides the global [session] output_template for the
	// matched streamers. Empty string is treated as "not set".
	OutputTemplate *string
	// DeletePartFiles overrides the global [processing] delete_part_files
	// setting for the matched streamers.
	DeletePartFiles *bool
	// SplitSize overrides the global [session] split_size for the matched
	// streamers. "off" or "0" disables size-based splitting. Empty = inherit.
	SplitSize *string
	// SplitDuration overrides the global [session] split_duration for the
	// matched streamers. "off" or "0" disables duration-based splitting. Empty = inherit.
	SplitDuration *string
	// ArchiveDir overrides the global [processing] archive_dir for the matched
	// streamers. "off" leaves their VODs in the Download folder; a path moves
	// them there instead. Empty = inherit the global setting.
	ArchiveDir *string
}

// streamerOptions is the effective options for a single streamer ID after
// merging defaults with any matching rules. Variant is empty when no rule
// overrides it; callers fall back to the global [session] variant.
// DeletePartFiles is nil when no rule overrides it; callers fall back to the
// global [processing] delete_part_files setting.
type streamerOptions struct {
	Monitor         bool
	Record          bool
	Variant         string
	OutputTemplate  string
	DeletePartFiles *bool
	// SplitSize is the raw override value ("2GB", "off", ""). Empty = inherit global.
	SplitSize string
	// SplitDuration is the raw override value ("1h30m", "off", ""). Empty = inherit global.
	SplitDuration string
	// ArchiveDir is the raw override value ("/mnt/nas", "off", ""). Empty = inherit global.
	ArchiveDir string
}

type globalLoggerConfig struct {
	Level     string `koanf:"level"`
	Directory string `koanf:"directory"`
	// Mode selects the output destination: off | file | console | both.
	// console writes to stdout/stderr (WARN/ERROR -> stderr).
	Mode string `koanf:"mode"`
}

type DaemonConfig struct {
	Logger            LoggerConfig `koanf:"logger"`
	Host              string       `koanf:"host"`
	Port              int          `koanf:"port"`
	AutoStart         bool         `koanf:"auto_start"`
	ExePath           string       `koanf:"exe_path"`
	BinaryRepo        string       `koanf:"bin_repo"`
	StreamersFilePath string       `koanf:"streamers_file_path"`
	ConfigFilePath    string       `koanf:"config_file_path"`
}

type SessionConfig struct {
	Logger      LoggerConfig `koanf:"logger"`
	DownloadDir string       `koanf:"download_dir"`
	Variant     string       `koanf:"variant"`
	// OutputTemplate is the directory layout, relative to DownloadDir, into which
	// a session's files are written. It supports placeholders like {platform},
	// {username} and {datetime} (see renderOutputDir). Empty falls back to
	// DefaultOutputTemplate. Can be overridden per streamer with an
	// output_template rule.
	OutputTemplate string `koanf:"output_template"`
	// SplitSize sets the maximum raw bytes per VOD file (e.g. "2GB"). When a
	// Part reaches this limit, a new Part is started and processing produces
	// vod1.mp4, vod2.mp4, … instead of vod.mp4.
	// Empty = no size-based splitting. Can be overridden per streamer.
	SplitSize string `koanf:"split_size"`
	// SplitDuration sets the maximum duration per VOD file (e.g. "1h30m").
	// When a Part reaches this limit, a new Part is started.
	// Empty = no duration-based splitting. Can be overridden per streamer.
	SplitDuration string `koanf:"split_duration"`
	// SanitizeFLV enables in-flight cleanup of FLV live streams: garbage and
	// duplicate (replayed) data are dropped and timestamps repaired during
	// recording, so troubled streams still produce clean, seekable VODs.
	// When the stream's codec configuration changes mid-Session (e.g. the
	// encoder switches resolution on a bad uplink), recording rotates to a
	// new Part and each configuration produces its own VOD file.
	// Default true; set false to dump the raw stream bytes unmodified.
	SanitizeFLV bool `koanf:"sanitize_flv"`
}

type ProcessingConfig struct {
	Logger LoggerConfig `koanf:"logger"`
	// DBPath sets the location of the processing queue database (processing.db).
	// Empty defaults to processing.db under the OS user config dir
	// (e.g. ~/.config/vodlabs/recorder/processing.db on Linux).
	DBPath string `koanf:"db_path"`
	// Encoder selects the video encoder for merge/re-encode jobs: "auto"
	// (default — first working hardware encoder, then software), "software"
	// (libx264), or a specific encoder id (e.g. "h264_videotoolbox",
	// "h264_nvenc"). A configured encoder that fails its runtime self-test
	// falls back to software with a warning on the job.
	Encoder string `koanf:"encoder"`
	// DeletePartFiles, when true, deletes the raw Part files once processing
	// finishes. Defaults to true. Can be overridden per streamer with a
	// delete_part_files rule.
	DeletePartFiles bool `koanf:"delete_part_files"`
	// ArchiveDir, when set, moves each VOD from the Download folder into this
	// long-term location (e.g. a network drive) after processing. Empty leaves
	// VODs in the Download folder. Must be a local/network directory path. Can
	// be overridden per streamer with an archive_dir rule ("off" keeps that
	// streamer's VODs in the Download folder).
	ArchiveDir string `koanf:"archive_dir"`
	// ScanDirs are read-only folders the VOD Archive also lists VODs from. They
	// are never written to and are not Storage roots. Each must be an absolute
	// local/network directory path that does not overlap the Download folder or
	// the Archive folder.
	ScanDirs []string `koanf:"scan_dirs"`
	// LowSpaceWarnGB is the free space, in gibibytes, below which a Storage root
	// is reported as running low. Absolute rather than a percentage, since the
	// question is whether the next VOD fits. 0 disables the warning; the hard
	// floors that refuse and stop recording are not affected.
	LowSpaceWarnGB int `koanf:"low_space_warn_gb"`
}

type CloudConfig struct {
	Logger LoggerConfig `koanf:"logger"`
}

type ScraperConfig struct {
	Logger LoggerConfig `koanf:"logger"`
}

type LoggerConfig struct {
	Level     string `koanf:"level"`
	Directory string `koanf:"directory"`
	FileName  string `koanf:"file_name"`
	// Mode selects the output destination: off | file | console | both.
	// Empty inherits the global [logger] mode (see ApplyLoggerDefaults).
	Mode string `koanf:"mode"`
}
