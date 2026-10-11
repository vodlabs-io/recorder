package hlsingest

// Mouflon segment names. Some Origins (Stripchat's doppiocdn) publish every
// media segment under a placeholder URI and carry its real URL in an
// #EXT-X-MOUFLON:URI tag ahead of it, with the file name's random field
// encrypted. An #EXT-X-MOUFLON:PSCH:v2:<key id> tag names the key; the service
// hands the keys over (Config.MouflonKeys), and the playlist is rewritten to
// the real URLs before it is parsed.
//
// v2 encryption: the field is reversed, base64-decoded, and XORed with the
// SHA-256 of the key.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	mouflonPSCHTag = "#EXT-X-MOUFLON:PSCH:"
	mouflonURITag  = "#EXT-X-MOUFLON:URI:"
	mouflonPartTag = "#EXT-X-PART:"
)

// mouflonName splits a segment URL around its encrypted field, which sits
// between the media sequence number and the timestamp:
// <stream>_<seq>_<field>_<unix>[_part<n>].mp4.
var mouflonName = regexp.MustCompile(`^(.*_\d+_)([A-Za-z0-9+/=]+)(_\d+(?:_part\d+)?\.mp4)$`)

// mouflonPlain is what a decrypted field must look like.
var mouflonPlain = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// errMouflonKey reports a playlist protected with a scheme or key this ingest
// was not given. Its placeholders would fetch nothing worth recording.
var errMouflonKey = errors.New("mouflon-protected playlist names no key the service supplied")

// restoreMouflon rewrites a media playlist's placeholder segment URIs to the
// real URLs its #EXT-X-MOUFLON:URI tags encrypt. A playlist without the tags
// is returned unchanged.
func restoreMouflon(body []byte, keys map[string]string) ([]byte, error) {
	if !bytes.Contains(body, []byte(mouflonURITag)) {
		return body, nil
	}
	var hash []byte
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), mouflonPSCHTag)
		if !ok {
			continue
		}
		scheme, id, _ := strings.Cut(rest, ":")
		if key, ok := keys[id]; ok && scheme == "v2" {
			h := sha256.Sum256([]byte(key))
			hash = h[:]
			break
		}
	}
	if hash == nil {
		return nil, errMouflonKey
	}

	pending := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, mouflonURITag); ok {
			real, err := decryptMouflonURI(rest, hash)
			if err != nil {
				return nil, err
			}
			pending = real
			continue
		}
		if pending == "" || trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, mouflonPartTag):
			// A low-latency part: its URI attribute is the placeholder.
			lines[i] = replaceURIAttr(line, pending)
			pending = ""
		case !strings.HasPrefix(trimmed, "#"):
			lines[i] = pending
			pending = ""
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// decryptMouflonURI returns the real URL a #EXT-X-MOUFLON:URI value encrypts.
func decryptMouflonURI(uri string, hash []byte) (string, error) {
	m := mouflonName.FindStringSubmatch(uri)
	if m == nil {
		return "", fmt.Errorf("mouflon URI %q has no encrypted name", uri)
	}
	field := []byte(m[2])
	for i, j := 0, len(field)-1; i < j; i, j = i+1, j-1 {
		field[i], field[j] = field[j], field[i]
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(string(field), "="))
	if err != nil {
		return "", fmt.Errorf("mouflon URI %q: %w", uri, err)
	}
	for i := range raw {
		raw[i] ^= hash[i%len(hash)]
	}
	if !mouflonPlain.Match(raw) {
		// The key no longer matches what the Origin encrypts with.
		return "", fmt.Errorf("mouflon URI %q did not decrypt: %w", uri, errMouflonKey)
	}
	return m[1] + string(raw) + m[3], nil
}

// replaceURIAttr swaps the URI attribute of a tag line for uri.
func replaceURIAttr(line, uri string) string {
	start := strings.Index(line, `URI="`)
	if start < 0 {
		return line
	}
	start += len(`URI="`)
	end := strings.IndexByte(line[start:], '"')
	if end < 0 {
		return line
	}
	return line[:start] + uri + line[start+end:]
}
