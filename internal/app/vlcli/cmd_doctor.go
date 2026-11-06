package vlcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/urfave/cli/v3"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// The service endpoints the daemon uses, and the variables that override them;
// keep in step with internal/cloud/client.go.
const (
	defaultAPIURL  = "https://api.vodlabs.io"
	defaultNATSURL = "wss://nats.vodlabs.io"
)

// serviceStateKey is where the daemon publishes whether it can reach the
// service; see internal/cloud/reachability.go.
const serviceStateKey = "daemon.service"

const doctorProbeTimeout = 10 * time.Second

func (a *App) doctorCommand() *cli.Command {
	return &cli.Command{
		Name:      "doctor",
		Usage:     "Check that this machine can record: the daemon, the service, and a platform",
		ArgsUsage: "[<url> | <platform> [<username>]]",
		Description: "Checks the daemon and its connection to the VodLabs service, and that this " +
			"machine reaches the service and GitHub. Given a platform, it also reaches the " +
			"platform's site; given a streamer this recorder tracks, it runs a real status check " +
			"from this machine. " +
			"Never starts a daemon. Exits 1 if any check fails.",
		Flags:     []cli.Flag{jsonFlag()},
		Arguments: streamerArgs(),
		Action:    a.runDoctor,
	}
}

// doctorCheck is one line of the report.
type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

func (a *App) runDoctor(ctx context.Context, cmd *cli.Command) error {
	if err := rejectExtraArgs(cmd, 0, "doctor"); err != nil {
		return err
	}
	target, err := parseDoctorTarget(cmd.StringArg("platform"), cmd.StringArg("username"))
	if err != nil {
		return err
	}

	report := doctorReport{OK: true}
	add := func(c doctorCheck) {
		report.Checks = append(report.Checks, c)
		report.OK = report.OK && c.OK
	}
	for _, c := range a.daemonChecks(ctx, target) {
		add(c)
	}
	for _, c := range networkChecks(ctx, a.cfg.BinaryRepo, target) {
		add(c)
	}

	if cmd.Bool("json") {
		if err := emitJSON(report); err != nil {
			return err
		}
	} else {
		w := newTable()
		for _, c := range report.Checks {
			mark := "ok"
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", mark, c.Name, c.Detail)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if !report.OK {
		return cli.Exit("", 1)
	}
	return nil
}

// doctorTarget is what to check beyond the service: nothing, a platform, or a
// streamer.
type doctorTarget struct {
	platform string
	ref      streamer.StreamerRef // set for a streamer
	site     string               // the page to reach, "" for none
}

func parseDoctorTarget(first, second string) (doctorTarget, error) {
	first, second = strings.TrimSpace(first), strings.TrimSpace(second)
	switch {
	case first == "":
		return doctorTarget{}, nil
	case looksLikeURL(first):
		if second != "" {
			return doctorTarget{}, fmt.Errorf("a URL names the streamer on its own; drop the extra %q", second)
		}
		u, err := url.Parse(first)
		if err != nil || u.Host == "" {
			return doctorTarget{}, fmt.Errorf("%q is not a URL", first)
		}
		return doctorTarget{ref: streamer.StreamerRef{URL: first}, site: u.Scheme + "://" + u.Host}, nil
	}
	t := doctorTarget{platform: strings.ToLower(first)}
	known := false
	for _, p := range vlconfig.Platforms() {
		if p.Name != t.platform {
			continue
		}
		known = true
		// A channel link through the service's own redirect says nothing about
		// reaching the platform.
		if u, err := url.Parse(p.ChannelURL); err == nil && u.Host != "" && !isServiceHost(u.Host) {
			t.site = u.Scheme + "://" + u.Host
		}
	}
	if !known {
		return doctorTarget{}, fmt.Errorf("%q is not a platform this recorder supports; see 'vod platforms'", first)
	}
	if second != "" {
		t.ref = streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: t.platform, Username: second}}
	}
	return t, nil
}

// daemonChecks asks the running daemon how it is doing. It never starts one.
func (a *App) daemonChecks(ctx context.Context, target doctorTarget) []doctorCheck {
	if err := pingDaemon(a.natsURL); err != nil {
		checks := []doctorCheck{{Name: "daemon", Detail: fmt.Sprintf(
			"not answering at %s; start it with 'vod status'", a.natsURL)}}
		if !target.ref.IsEmpty() {
			checks = append(checks, doctorCheck{Name: "check", Detail: "needs the daemon"})
		}
		return checks
	}
	ctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	nc, kv, err := initNATS(ctx, a.natsURL, cancel, func() bool { return true })
	if err != nil {
		return []doctorCheck{{Name: "daemon", Detail: err.Error()}}
	}
	defer nc.Close()
	a.nc = nc

	daemonVer, _ := vlstate.GetDaemonVersion(ctx, kv)
	checks := []doctorCheck{{Name: "daemon", OK: true,
		Detail: fmt.Sprintf("answering at %s, version %s", a.natsURL, dash(daemonVer))}}

	version := doctorCheck{Name: "version", OK: true, Detail: "accepted by the service"}
	if upstream, err := vlstate.GetUpstreamVersion(ctx, kv); err == nil {
		switch {
		case upstream.UpdateRequired && daemonVer == "devel":
			version = doctorCheck{Name: "version", Detail: "the service refuses development builds; " +
				"install a release with: curl -fsSL https://vodlabs.io/install.sh | sh"}
		case upstream.UpdateRequired:
			version = doctorCheck{Name: "version", Detail: "the service no longer serves this version; run 'vod update'"}
		case upstream.RecommendedVersion != "" && upstream.RecommendedVersion != daemonVer:
			version.Detail += "; " + upstream.RecommendedVersion + " is available ('vod update')"
		}
	} else {
		version.Detail = "not checked yet"
	}
	checks = append(checks, version)
	checks = append(checks, serviceCheck(ctx, kv))
	if target.platform != "" {
		checks = append(checks, a.offeredCheck(target.platform))
	}

	if !target.ref.IsEmpty() {
		checks = append(checks, a.statusCheck(target.ref))
	}
	return checks
}

// serviceCheck reports what the daemon last said about its connection to the
// service.
func serviceCheck(ctx context.Context, kv jetstream.KeyValue) doctorCheck {
	c := doctorCheck{Name: "service"}
	entry, err := kv.Get(ctx, serviceStateKey)
	if err != nil {
		c.OK, c.Detail = true, "no report from the daemon yet"
		return c
	}
	var st struct {
		Reachable      bool   `json:"reachable"`
		Error          string `json:"error"`
		SubscribeError string `json:"subscribe_error"`
	}
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		c.Detail = "unreadable report: " + err.Error()
		return c
	}
	switch {
	case !st.Reachable:
		c.Detail = "the daemon cannot reach the service: " + dash(st.Error)
	case st.SubscribeError != "":
		c.Detail = "connected, but streamers are not being checked: " + st.SubscribeError
	default:
		c.OK, c.Detail = true, "connected"
	}
	return c
}

// offeredCheck reports whether the service offers the platform to this daemon,
// which can differ from the platforms built into it.
func (a *App) offeredCheck(platform string) doctorCheck {
	c := doctorCheck{Name: "platform"}
	offered, err := vlapi.Request[any, []vlconfig.Platform](a.nc, "platforms", nil)
	if err != nil {
		c.Detail = err.Error()
		return c
	}
	if offered != nil {
		for _, p := range *offered {
			if p.Name == platform {
				c.OK, c.Detail = true, vlconfig.DisplayNameFor(platform)+" is offered by the service"
				return c
			}
		}
	}
	c.Detail = vlconfig.DisplayNameFor(platform) + " is not offered by the service right now, so it cannot be recorded"
	return c
}

// isServiceHost reports whether host is the VodLabs service's own.
func isServiceHost(host string) bool {
	for _, raw := range []string{defaultAPIURL, os.Getenv("VLREC_CLOUD_API_URL")} {
		if u, err := url.Parse(raw); err == nil && u.Host != "" && strings.EqualFold(u.Host, host) {
			return true
		}
	}
	return false
}

// statusCheck runs a real check of one streamer from this machine.
func (a *App) statusCheck(ref streamer.StreamerRef) doctorCheck {
	c := doctorCheck{Name: "check " + describeRef(ref)}
	status, err := a.checkNow(ref)
	switch {
	case errors.Is(err, vlapi.ErrPlatformUnreachable):
		c.Detail = strings.TrimPrefix(err.Error(), vlapi.ErrPlatformUnreachable.Error()+": ")
	case err != nil && strings.Contains(err.Error(), "not in your subscriptions"):
		c.Detail = "the service checks only streamers this recorder tracks; add it with 'vod streamer add' first"
	case err != nil:
		c.Detail = err.Error()
	case status == "checking":
		c.OK, c.Detail = true, "queued; this daemon or this platform's login cannot report the result"
	default:
		c.OK, c.Detail = true, status
	}
	return c
}

// networkChecks reaches the service, GitHub and the target's site from this
// process. Any HTTP answer counts: the question is whether the network lets the
// request through, not what the server makes of it.
func networkChecks(ctx context.Context, binRepo string, target doctorTarget) []doctorCheck {
	type probe struct{ name, url string }
	natsURL := envOr("VLREC_CLOUD_NATS_URL", defaultNATSURL)
	natsURL = strings.Replace(strings.Replace(natsURL, "wss://", "https://", 1), "ws://", "http://", 1)
	probes := []probe{
		{"service api", strings.TrimRight(envOr("VLREC_CLOUD_API_URL", defaultAPIURL), "/") + "/"},
		{"service nats", natsURL},
	}
	if binRepo != "" {
		probes = append(probes, probe{"updates", strings.TrimRight(binRepo, "/") + "/releases/latest"})
	}
	if target.site != "" {
		name := target.platform
		if name == "" {
			name = "site"
		}
		probes = append(probes, probe{name, target.site + "/"})
	}

	client := &http.Client{
		Timeout: doctorProbeTimeout,
		// The first answer is the one that says the network let it through.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	out := make([]doctorCheck, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = reach(ctx, client, p.name, p.url)
		}()
	}
	wg.Wait()
	return out
}

func reach(ctx context.Context, client *http.Client, name, rawURL string) doctorCheck {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return doctorCheck{Name: name, Detail: err.Error()}
	}
	res, err := client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return doctorCheck{Name: name, Detail: fmt.Sprintf("%s: %v", rawURL, err)}
	}
	res.Body.Close()
	return doctorCheck{Name: name, OK: true, Detail: fmt.Sprintf("%s answered %d in %s",
		rawURL, res.StatusCode, time.Since(start).Round(time.Millisecond))}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
