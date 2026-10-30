package sushitrain

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/syncthing"
)

// Synctrain logs through the handler the engine installs as slog's default, as the engine's packages do. The level of
// each package (log facility) applies to its lines, and the log viewer and support bundle show the lines the handler
// recorded.

func init() {
	syncthing.RegisterLogFacility("Synctrain")
}

var deviceIDTailRegexp = regexp.MustCompile("(-[A-Z0-9]{7}){7}")
var ipHeadRegexp = regexp.MustCompile("(([0-9]{1,3}\\.){3})|(([0-9a-fA-F]{1,4}:){4})")
var pathsRegexp = regexp.MustCompile("/Users/[^/]+/")
var uuidTailRegexp = regexp.MustCompile("-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}")

func redactLog(line string) string {
	line = deviceIDTailRegexp.ReplaceAllString(line, "•••")
	line = ipHeadRegexp.ReplaceAllString(line, "•••.•••.•••.")
	line = pathsRegexp.ReplaceAllString(line, "/Users/•••/")
	line = uuidTailRegexp.ReplaceAllString(line, "-•••")
	return line
}

// configureLogging writes log lines to out, sets the level of all facilities from SUSHITRAIN_MIN_LOG_LEVEL (WARN by
// default, INFO when the app saves its log), then the levels of single facilities from STTRACE, as Syncthing does
// ("model,protocol:WARN"). It returns the level of all facilities.
func configureLogging(out io.Writer, saveLog bool) slog.Level {
	syncthing.SetLogOutput(out)
	minLevel := slog.LevelWarn
	if envMinLevel, present := os.LookupEnv("SUSHITRAIN_MIN_LOG_LEVEL"); present {
		switch envMinLevel {
		case "INFO":
			minLevel = slog.LevelInfo
		case "WARN":
			minLevel = slog.LevelWarn
		case "ERROR":
			minLevel = slog.LevelError
		case "DEBUG":
			minLevel = slog.LevelDebug
		default:
			minLevel = slog.LevelInfo
		}
	} else if saveLog {
		minLevel = slog.LevelInfo
	}
	syncthing.SetLogLevels(minLevel, os.Getenv("STTRACE"))
	return minLevel
}

type logFacility struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Level       int    `json:"level"`
}

// LogFacilitiesJSON returns the log facilities, sorted by name, as a JSON array of objects with their name,
// description and level (as in slog: DEBUG is -4, INFO 0, WARN 4 and ERROR 8).
func (c *Client) LogFacilitiesJSON() ([]byte, error) {
	descrs := syncthing.LogFacilities()
	levels := syncthing.LogLevels()
	facilities := make([]logFacility, 0, len(descrs))
	for _, name := range slices.Sorted(maps.Keys(descrs)) {
		facilities = append(facilities, logFacility{Name: name, Description: descrs[name], Level: int(levels[name])})
	}
	return json.Marshal(facilities)
}

// DefaultLogLevel returns the level of facilities whose own level wasn't set
func (c *Client) DefaultLogLevel() int {
	return int(c.defaultLogLevel)
}

// SetLogLevel sets the level of one log facility. It takes effect at once, and lasts until the app quits.
func (c *Client) SetLogLevel(facility string, level int) {
	syncthing.SetLogLevel(facility, slog.Level(level))
}

// ResetLogLevels sets every log facility to the default level
func (c *Client) ResetLogLevels() {
	for name, level := range syncthing.LogLevels() {
		if level != c.defaultLogLevel {
			syncthing.SetLogLevel(name, c.defaultLogLevel)
		}
	}
}

// ChangedLogLevels returns the facilities whose level differs from the default, with their levels, in the format of
// STTRACE ("model:DEBUG,protocol:WARN"), for ApplyLogLevels to restore
func (c *Client) ChangedLogLevels() string {
	levels := syncthing.LogLevels()
	var changed []string
	for _, name := range slices.Sorted(maps.Keys(levels)) {
		if levels[name] != c.defaultLogLevel {
			changed = append(changed, name+":"+levels[name].String())
		}
	}
	return strings.Join(changed, ",")
}

// ApplyLogLevels sets the levels of the facilities named in levels, in the format ChangedLogLevels returns
func (c *Client) ApplyLogLevels(levels string) {
	syncthing.SetLogLevels(c.defaultLogLevel, levels)
}

type logLine struct {
	When     int64  `json:"when"` // milliseconds since the Unix epoch
	Level    int    `json:"level"`
	Facility string `json:"facility"`
	Message  string `json:"message"`
}

// LogLinesJSON returns the last lines logged, oldest first and redacted, as a JSON array of objects with their time,
// level, facility (empty when unknown) and message
func (c *Client) LogLinesJSON() ([]byte, error) {
	recorded := syncthing.LogLines(time.Time{})
	lines := make([]logLine, len(recorded))
	for i, line := range recorded {
		lines[i] = logLine{
			When:     line.When.UnixMilli(),
			Level:    int(line.Level),
			Facility: line.Facility,
			Message:  redactLog(line.Message),
		}
	}
	return json.Marshal(lines)
}

// writeLogLines writes the last lines logged, oldest first and redacted
func writeLogLines(to io.Writer) error {
	for _, line := range syncthing.LogLines(time.Time{}) {
		when := line.When.Format("2006-01-02 15:04:05.000")
		if _, err := fmt.Fprintln(to, when, line.Level, redactLog(line.Message)); err != nil {
			return err
		}
	}
	return nil
}
