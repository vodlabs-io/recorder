package vlconfig

import (
	"log/slog"

	"github.com/knadh/koanf/v2"
)

// applyTo merges any fields set on o into out. Later rules calling applyTo
// override earlier ones only for the fields they explicitly set.
func (o RuleOptions) applyTo(out *streamerOptions) {
	if o.Monitor != nil {
		out.Monitor = *o.Monitor
	}
	if o.Record != nil {
		out.Record = *o.Record
	}
	if o.Variant != nil {
		out.Variant = *o.Variant
	}
	if o.OutputTemplate != nil {
		out.OutputTemplate = *o.OutputTemplate
	}
	if o.DeletePartFiles != nil {
		out.DeletePartFiles = o.DeletePartFiles
	}
	if o.SplitSize != nil {
		out.SplitSize = *o.SplitSize
	}
	if o.SplitDuration != nil {
		out.SplitDuration = *o.SplitDuration
	}
	if o.ArchiveDir != nil {
		out.ArchiveDir = *o.ArchiveDir
	}
}

// writeTo appends the set option fields, in declaration order, to kvs and
// returns the result. Keeping every option listed here in one place means
// adding a new option only touches this file.
func (o RuleOptions) writeTo(kvs []tomlKV) []tomlKV {
	if o.Monitor != nil {
		kvs = append(kvs, tomlKV{"monitor", *o.Monitor})
	}
	if o.Record != nil {
		kvs = append(kvs, tomlKV{"record", *o.Record})
	}
	if o.Variant != nil {
		kvs = append(kvs, tomlKV{"variant", *o.Variant})
	}
	if o.OutputTemplate != nil {
		kvs = append(kvs, tomlKV{"output_template", *o.OutputTemplate})
	}
	if o.DeletePartFiles != nil {
		kvs = append(kvs, tomlKV{"delete_part_files", *o.DeletePartFiles})
	}
	if o.SplitSize != nil {
		kvs = append(kvs, tomlKV{"split_size", *o.SplitSize})
	}
	if o.SplitDuration != nil {
		kvs = append(kvs, tomlKV{"split_duration", *o.SplitDuration})
	}
	if o.ArchiveDir != nil {
		kvs = append(kvs, tomlKV{"archive_dir", *o.ArchiveDir})
	}
	return kvs
}

// ruleOptionKeys names every key readFrom consumes, so parseRule can tell an
// option from a platform's username list. Keep it in step with readFrom.
func ruleOptionKeys() []string {
	return []string{
		"monitor",
		"record",
		"variant",
		"output_template",
		"delete_part_files",
		"split_size",
		"split_duration",
		"archive_dir",
	}
}

// readFrom decodes a koanf rule node into o.
func (o *RuleOptions) readFrom(k *koanf.Koanf) {
	if k.Exists("monitor") {
		v := k.Bool("monitor")
		o.Monitor = &v
	}
	if k.Exists("record") {
		v := k.Bool("record")
		o.Record = &v
	}
	if k.Exists("variant") {
		v := k.String("variant")
		o.Variant = &v
	}
	if k.Exists("output_template") {
		v := k.String("output_template")
		o.OutputTemplate = &v
	}
	if k.Exists("delete_part_files") {
		v := k.Bool("delete_part_files")
		o.DeletePartFiles = &v
	}
	if k.Exists("split_size") {
		v := k.String("split_size")
		if _, err := ParseSplitSize(v); err != nil {
			slog.Warn("Ignoring invalid split_size in rule", "value", v, "error", err)
		} else {
			o.SplitSize = &v
		}
	}
	if k.Exists("split_duration") {
		v := k.String("split_duration")
		if _, err := ParseSplitDuration(v); err != nil {
			slog.Warn("Ignoring invalid split_duration in rule", "value", v, "error", err)
		} else {
			o.SplitDuration = &v
		}
	}
	if k.Exists("archive_dir") {
		v := k.String("archive_dir")
		if err := ValidateArchiveDir(v); err != nil {
			slog.Warn("Ignoring invalid archive_dir in rule", "value", v, "error", err)
		} else {
			o.ArchiveDir = &v
		}
	}
}

// anySet reports whether any option field is non-nil.
func (o RuleOptions) anySet() bool {
	return o.Monitor != nil || o.Record != nil || o.Variant != nil ||
		o.OutputTemplate != nil || o.DeletePartFiles != nil ||
		o.SplitSize != nil || o.SplitDuration != nil || o.ArchiveDir != nil
}

// equal reports whether o and other set exactly the same option fields to the
// same values. Rules carrying equal options can be consolidated into one.
func (o RuleOptions) equal(other RuleOptions) bool {
	return boolPtrEqual(o.Monitor, other.Monitor) &&
		boolPtrEqual(o.Record, other.Record) &&
		strPtrEqual(o.Variant, other.Variant) &&
		strPtrEqual(o.OutputTemplate, other.OutputTemplate) &&
		boolPtrEqual(o.DeletePartFiles, other.DeletePartFiles) &&
		strPtrEqual(o.SplitSize, other.SplitSize) &&
		strPtrEqual(o.SplitDuration, other.SplitDuration) &&
		strPtrEqual(o.ArchiveDir, other.ArchiveDir)
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func strPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
