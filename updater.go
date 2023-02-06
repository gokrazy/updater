// Package updater implements updating the different parts of a running gokrazy
// installation (boot/root file systems and MBR).
package updater

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
)

// ErrUpdateHandlerNotImplemented is returned when the requested update
// destination is not yet implemented on the target device. Callers can
// programmatically distinguish this error to print an according message and
// possibly proceed with the update.
var ErrUpdateHandlerNotImplemented = errors.New("update handler not implemented")

// A HTTPDoer is satisfied by any *http.Client, but also easy to implement in
// case extra middleware is desired.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Target represents a gokrazy installation to be updated.
type Target struct {
	doer HTTPDoer

	baseURL  string
	supports []string
}

// NewTarget queries the target for supported update protocol features and
// returns a ready-to-use updater Target.
func NewTarget(baseURL string, httpClient HTTPDoer) (*Target, error) {
	supports, err := targetSupports(baseURL, httpClient)
	if err != nil {
		return nil, err
	}
	return &Target{
		baseURL:  baseURL,
		doer:     httpClient,
		supports: supports,
	}, nil
}

// A ProtocolFeature represents an optionally available feature of the update
// protocol, i.e. features that might possibly be missing in older gokrazy
// installations.
type ProtocolFeature string

const (
	// ProtocolFeaturePARTUUID signals that the target understands the PARTUUID=
	// Linux kernel parameter and uses it in its cmdline.txt bootloader config,
	// i.e. is ready to accept an update that is using PARTUUID, too.
	ProtocolFeaturePARTUUID ProtocolFeature = "partuuid"

	// ProtocolFeatureUpdateHash signals that the target understands the
	// X-Gokrazy-Update-Hash HTTP header and at least the “crc32” value, which
	// is significantly faster than SHA256, which is used by default.
	ProtocolFeatureUpdateHash ProtocolFeature = "updatehash"
)

// Supports returns whether the target is known to support the specified update
// protocol feature.
func (t *Target) Supports(feature ProtocolFeature) bool {
	for _, f := range t.supports {
		if f == string(feature) {
			return true
		}
	}
	return false
}

// StreamTo streams from the specified io.Reader to the specified destination:
//
//   - mbr: stream content directly onto the root block device
//   - root: stream content to the currently inactive root partition
//   - boot: stream content to the boot partition
//
// When updating only the boot partition and not also the root partition
// (e.g. in gokrazy’s Continuous Integration), the following suffix should be
// used:
//
//   - bootonly: stream content to the boot partition, then update the boot
//     partition so that the currently active root stays active.
//
// You can keep track of progress by passing in an io.TeeReader(r,
// &countingWriter{}).
func (t *Target) StreamTo(dest string, r io.Reader) error {
	updateHash := t.Supports("updatehash")
	var hash hash.Hash
	if updateHash {
		hash = crc32.NewIEEE()
	} else {
		hash = sha256.New()
	}
	req, err := http.NewRequest(http.MethodPut, t.baseURL+"update/"+dest, io.TeeReader(r, hash))
	if err != nil {
		return err
	}
	if updateHash {
		req.Header.Set("X-Gokrazy-Update-Hash", "crc32")
	}
	resp, err := t.doer.Do(req)
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("unexpected HTTP status code: got %v, want %v (body %q)", resp.Status, want, string(body))
	}
	remoteHash, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if bytes.HasPrefix(remoteHash, []byte("<!DOCTYPE html>")) {
		return ErrUpdateHandlerNotImplemented
	}
	decoded := make([]byte, hex.DecodedLen(len(remoteHash)))
	n, err := hex.Decode(decoded, remoteHash)
	if err != nil {
		return err
	}
	if got, want := decoded[:n], hash.Sum(nil); !bytes.Equal(got, want) {
		return fmt.Errorf("unexpected checksum: got %x, want %x", got, want)
	}
	return nil
}

// Put streams a file to the specified HTTP endpoint, without verifying its
// hash. This is not suited for updating the system, which should be done via
// StreamTo() instead. This function is useful for the /uploadtemp/ handler.
func (t *Target) Put(dest string, r io.Reader) error {
	req, err := http.NewRequest(http.MethodPut, t.baseURL+dest, r)
	if err != nil {
		return err
	}
	resp, err := t.doer.Do(req)
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("/uploadtemp/ handler not found, is your gokrazy installation too old?")
		}
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("unexpected HTTP status code: got %v, want %v (body %q)", resp.Status, want, strings.TrimSpace(string(body)))
	}
	return nil
}

// Switch changes the active root partition from the currently running root
// partition to the currently inactive root partition.
func (t *Target) Switch() error {
	req, err := http.NewRequest("POST", t.baseURL+"update/switch", nil)
	if err != nil {
		return err
	}
	resp, err := t.doer.Do(req)
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("unexpected HTTP status code: got %d, want %d (body %q)", got, want, string(body))
	}
	return nil
}

// Testboot marks the inactive root partition to be tested upon the next boot,
// and made active if the test boot succeeds.
func (t *Target) Testboot() error {
	req, err := http.NewRequest("POST", t.baseURL+"update/testboot", nil)
	if err != nil {
		return err
	}
	resp, err := t.doer.Do(req)
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("unexpected HTTP status code: got %d, want %d (body %q)", got, want, string(body))
	}
	return nil
}

// Reboot reboots the target, picking up the updated partitions.
func (t *Target) Reboot() error {
	req, err := http.NewRequest("POST", t.baseURL+"reboot", nil)
	if err != nil {
		return err
	}
	resp, err := t.doer.Do(req)
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("unexpected HTTP status code: got %d, want %d (body %q)", got, want, string(body))
	}
	return nil
}

// Divert makes gokrazy use the temporary binary (diversion) instead of
// /user/<basename>. Includes an automatic service restart.
func (t *Target) Divert(path, diversion string, serviceFlags, commandLineFlags []string) error {
	u, err := url.Parse(t.baseURL + "divert")
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Path      string
		Diversion string
		Flags     []string
	}{
		Path:      path,
		Diversion: diversion,
		Flags:     append(serviceFlags, commandLineFlags...),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if err != nil {
		return err
	}
	var resp *http.Response
	resp, err = t.doer.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusBadRequest {
		// A BadRequest could indicate that the server is running an older
		// version of gokrazy which took diversion options as query
		// parameters. Try this approach before giving up.
		if len(commandLineFlags) > 0 {
			return fmt.Errorf("running version of gokrazy does not support command line arguments; try upgrading")
		}
		values := u.Query()
		values.Set("path", path)
		values.Set("diversion", diversion)
		u.RawQuery = values.Encode()
		req, err := http.NewRequest("POST", u.String(), nil)
		if err != nil {
			return err
		}
		resp, err = t.doer.Do(req)
		if err != nil {
			return err
		}
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("unexpected HTTP status code: got %d, want %d (body %q)", got, want, strings.TrimSpace(string(body)))
	}
	return nil
}

func targetSupports(baseURL string, client HTTPDoer) ([]string, error) {
	req, err := http.NewRequest("GET", baseURL+"update/features", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		// Target device does not support /features handler yet, so no features
		// are supported.
		return nil, nil
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected HTTP status code: got %d, want %d (body %q)", got, want, string(body))
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(body)), ","), nil
}
