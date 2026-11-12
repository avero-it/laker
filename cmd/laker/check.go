package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/pkg/config"
)

// checkResultMarker prefixes the one line of stdout that carries the result.
const checkResultMarker = "LAKER_CHECK_RESULT "

const checkTimeout = 25 * time.Second

var checkCodeRE = regexp.MustCompile(`(ERR|FTL)[0-9A-Fa-f]{3}`)

// checkResult is what `laker serve --check` reports: whether the init phase
// succeeds and, if not, the phase and the code that say where it stopped.
type checkResult struct {
	OK         bool             `json:"ok"`
	Phase      string           `json:"phase,omitempty"` // load | validate | datadir | lake
	Code       string           `json:"code,omitempty"`
	Message    string           `json:"message,omitempty"`
	ConfigHash string           `json:"config_hash,omitempty"`
	Lake       *domain.LakeInfo `json:"lake,omitempty"`
}

func (r checkResult) fail(phase string, err error) checkResult {
	r.OK = false
	r.Phase = phase
	r.Message = err.Error()
	r.Code = checkCodeRE.FindString(r.Message)
	return r
}

// commandCheck runs the init phase without serving: nothing is scheduled and
// nothing is written. Exit code: 0 ok, 1 the init fails.
func commandCheck(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	res := runCheck(ctx, path)

	b, err := json.Marshal(res)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR020: cannot encode the check result: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(checkResultMarker + string(b))
	if !res.OK {
		os.Exit(1)
	}
}

func runCheck(ctx context.Context, path string) checkResult {
	var res checkResult

	path, err := config.Locate(path)
	if err != nil {
		return res.fail("load", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return res.fail("load", fmt.Errorf("ERR021: failed to load config: %w", err))
	}
	res.ConfigHash = config.Hash(cfg)

	if err := config.Validate(cfg); err != nil {
		return res.fail("validate", fmt.Errorf("ERR022: failed to validate config: %w", err))
	}

	if err := checkDataDir(cfg.Common.DataDir); err != nil {
		return res.fail("datadir", err)
	}

	lake, err := buildLake(cfg, zerolog.Nop())
	if err != nil {
		return res.fail("lake", err)
	}
	info, err := lake.Check(ctx)
	res.Lake = &info
	if err != nil {
		return res.fail("lake", err)
	}

	res.OK = true
	return res
}

// checkDataDir accepts a writable directory, or a missing one whose parent is
// writable (serve creates it).
func checkDataDir(dir string) error {
	st, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := syscall.Access(filepath.Dir(dir), 0x2 /* W_OK */); err != nil {
			return fmt.Errorf("ERR023: data directory %s does not exist and cannot be created: %w", dir, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("ERR024: data directory %s: %w", dir, err)
	case !st.IsDir():
		return fmt.Errorf("ERR025: data directory %s is not a directory", dir)
	}
	if err := syscall.Access(dir, 0x2 /* W_OK */); err != nil {
		return fmt.Errorf("ERR026: data directory %s is not writable: %w", dir, err)
	}
	return nil
}
