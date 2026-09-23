package vlconfig

// ApplyLoggerDefaults fills empty per-component Level/Directory/Mode fields from
// the global [logger] section. Per-component values already set are preserved.
func (c *AppConfig) ApplyLoggerDefaults() {
	apply := func(l *LoggerConfig) {
		if l.Level == "" {
			l.Level = c.Logger.Level
		}
		if l.Directory == "" {
			l.Directory = c.Logger.Directory
		}
		if l.Mode == "" {
			l.Mode = c.Logger.Mode
		}
	}
	apply(&c.Daemon.Logger)
	apply(&c.Session.Logger)
	apply(&c.Processing.Logger)
	apply(&c.Cloud.Logger)
	apply(&c.Scraper.Logger)
}
