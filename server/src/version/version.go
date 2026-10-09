// Package version implements the server-side update check. It reads the
// server's own version from the bundled web/version.json (the same asset the
// Flutter web build produces) and periodically asks cosmos-cloud.io whether a
// newer release exists, logging when one is found.
package version

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
	"github.com/go-co-op/gocron/v2"
)

// updateCheckURL is the endpoint that returns the latest published server
// version as JSON ({"version": "x.y.z"}).
const updateCheckURL = "https://cosmos-cloud.io/update-check/plurality/server"

// NewVersionAvailable is set to true once a newer server version has been
// detected by the periodic check. Kept around so the rest of the app (or an
// API endpoint) can surface it later.
var NewVersionAvailable bool

// versionFile is the server's own bundled web/version.json (Flutter format).
type versionFile struct {
	Version string `json:"version"`
}

// updateCheckResponse is the payload returned by the cosmos-cloud.io
// update-check endpoint, e.g. {"latest":"2.0.7","link":"https://..."}.
type updateCheckResponse struct {
	Latest string `json:"latest"`
	Link   string `json:"link"`
}

// GetServerVersion reads the server's version from web/version.json located
// next to the executable. Returns "" if it cannot be determined.
func GetServerVersion() string {
	ex, err := os.Executable()
	if err != nil {
		utils.Error("checkVersion - locate executable", err)
		return ""
	}
	path := filepath.Join(filepath.Dir(ex), "web", "version.json")

	data, err := os.ReadFile(path)
	if err != nil {
		utils.Error("checkVersion - read "+path, err)
		return ""
	}

	var v versionFile
	if err := json.Unmarshal(data, &v); err != nil {
		utils.Error("checkVersion - parse version.json", err)
		return ""
	}

	return v.Version
}

// CompareSemver compares two semantic version strings. It returns -1 if a < b,
// 0 if they are equal, and 1 if a > b. Any pre-release/build suffix (after '-'
// or '+') is stripped, missing components are treated as 0, and non-numeric
// components compare as 0.
func CompareSemver(a, b string) int {
	pa := parseSemver(a)
	pb := parseSemver(b)

	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}

	for i := 0; i < n; i++ {
		var na, nb int
		if i < len(pa) {
			na = pa[i]
		}
		if i < len(pb) {
			nb = pb[i]
		}
		if na < nb {
			return -1
		}
		if na > nb {
			return 1
		}
	}
	return 0
}

func parseSemver(v string) []int {
	core := strings.FieldsFunc(strings.TrimSpace(v), func(r rune) bool {
		return r == '-' || r == '+'
	})
	if len(core) == 0 {
		return nil
	}
	parts := strings.Split(core[0], ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, _ := strconv.Atoi(strings.TrimSpace(p))
		out[i] = n
	}
	return out
}

// checkVersion fetches the latest published server version and logs whether an
// update is available.
func checkVersion() {
	NewVersionAvailable = false

	myVersion := GetServerVersion()
	if myVersion == "" {
		utils.Error("checkVersion - could not determine server version", nil)
		return
	}

	// Cache-bust so intermediary caches/CDNs don't serve a stale version.
	url := fmt.Sprintf("%s?cacheBust=%d", updateCheckURL, time.Now().UnixMilli())
	resp, err := utils.HTTPClient.Get(url)
	if err != nil {
		utils.Error("checkVersion - request", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		utils.Error("checkVersion - unexpected status "+resp.Status, nil)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.Error("checkVersion - read response", err)
		return
	}

	var latest updateCheckResponse
	if err := json.Unmarshal(body, &latest); err != nil {
		utils.Error("checkVersion - parse response", err)
		return
	}
	if latest.Latest == "" {
		utils.Error("checkVersion - empty latest version", nil)
		return
	}

	if CompareSemver(myVersion, latest.Latest) == -1 {
		utils.Log("New version available: %s (current: %s) - %s", latest.Latest, myVersion, latest.Link)
		NewVersionAvailable = true
	} else {
		utils.Log("No new version available (current: %s)", myVersion)
	}
}

// Init schedules the periodic update check at a host-stable random time of day
// and runs one check shortly after startup. Pass the scheduler from the main
// package so all jobs share a single gocron instance.
func Init() {
	s, err := gocron.NewScheduler()
	if err != nil {
		utils.Error("version.Init - scheduler", err)
		return
	}

	// Spread load across servers: derive a deterministic-but-arbitrary time of
	// day from the hostname so every install hits the endpoint at a different
	// minute rather than all at once.
	hostname, _ := os.Hostname()
	h := fnv.New32a()
	h.Write([]byte(hostname))
	sum := h.Sum32()
	hour := int(sum % 24)
	minute := int((sum / 24) % 60)
	at := fmt.Sprintf("%02d:%02d", hour, minute)

	_, err = s.NewJob(
		gocron.DailyJob(1, gocron.NewAtTimes(
			gocron.NewAtTime(uint(hour), uint(minute), 0),
		)),
		gocron.NewTask(checkVersion),
	)
	if err != nil {
		utils.Error("version.Init - schedule job", err)
		return
	}

	s.Start()
	utils.Log("Version check scheduled daily at %s", at)

	// Run an initial check in the background so we know on boot, without
	// blocking startup.
	go func() {
		time.Sleep(30 * time.Second)
		checkVersion()
	}()
}
