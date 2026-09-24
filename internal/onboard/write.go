package onboard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
)

// ConfigMode is the permission the configuration is written with. It holds no
// secret and is meant to be checked in.
const ConfigMode = 0o644

// ErrExists is what [Write] refuses an existing file with when it is not
// asked to overwrite it.
var ErrExists = errors.New("already exists")

// Write validates the configuration the way `hearsay config validate` does,
// then writes it to configPath and the env file to envPath, mode [EnvMode].
// It returns what the configuration loads as.
//
// Without overwrite, a target that exists is refused with [ErrExists] and
// neither file is written, and each file is created exclusively, so one that
// appears in the meantime is not overwritten either. With it, each file is
// written beside its target and renamed over it, so a failed write leaves the
// old file whole, and an existing env file comes out mode 0600 whatever it
// was before.
func Write(res Result, configPath, envPath string, overwrite bool) (config.Repo, error) {
	if ext := filepath.Ext(configPath); ext != ".yaml" && ext != ".yml" {
		return config.Repo{}, fmt.Errorf("%s: the configuration is a .yaml or .yml file", configPath)
	}
	if same, err := samePath(configPath, envPath); err != nil {
		return config.Repo{}, err
	} else if same {
		return config.Repo{}, fmt.Errorf("%s is both the configuration and the env file: the secrets go in a file of their own", configPath)
	}
	if !overwrite {
		var errs []error
		for _, p := range []string{configPath, envPath} {
			if _, err := os.Lstat(p); err == nil {
				errs = append(errs, fmt.Errorf("%s %w", p, ErrExists))
			} else if !errors.Is(err, fs.ErrNotExist) {
				return config.Repo{}, fmt.Errorf("checking %s: %w", p, err)
			}
		}
		if err := errors.Join(errs...); err != nil {
			return config.Repo{}, err
		}
	}

	repo, err := validate(res.Config)
	if err != nil {
		return config.Repo{}, err
	}
	if err := writeFile(envPath, res.Env, EnvMode, overwrite); err != nil {
		return config.Repo{}, err
	}
	if err := writeFile(configPath, res.Config, ConfigMode, overwrite); err != nil {
		if !overwrite {
			// The env file was created a moment ago and is nobody's yet; a
			// second run should not be refused because of it.
			_ = os.Remove(envPath)
		}
		return config.Repo{}, err
	}
	return repo, nil
}

// validate loads the configuration from a scratch directory, so nothing is
// written where the person asked until it is known to load.
func validate(body []byte) (config.Repo, error) {
	dir, err := os.MkdirTemp("", "hearsay-init-")
	if err != nil {
		return config.Repo{}, fmt.Errorf("making a scratch directory to validate in: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "hearsay.yaml")
	if err := os.WriteFile(path, body, ConfigMode); err != nil {
		return config.Repo{}, fmt.Errorf("writing the configuration to validate it: %w", err)
	}
	repo, err := config.Load(path)
	if err != nil {
		return config.Repo{}, fmt.Errorf("the generated configuration does not validate, which is a bug in hearsay init: %w", err)
	}
	return repo, nil
}

func samePath(a, b string) (bool, error) {
	absA, err := filepath.Abs(a)
	if err != nil {
		return false, fmt.Errorf("resolving %s: %w", a, err)
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		return false, fmt.Errorf("resolving %s: %w", b, err)
	}
	if absA == absB {
		return true, nil
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib), nil
}

func writeFile(path string, body []byte, mode os.FileMode, overwrite bool) error {
	if !overwrite {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s %w", path, ErrExists)
		}
		if err != nil {
			return fmt.Errorf("creating %s: %w", path, err)
		}
		if _, err := f.Write(body); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return fmt.Errorf("writing %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("writing %s: %w", path, err)
		}
		return nil
	}

	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+strings.TrimPrefix(base, ".")+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating a file beside %s: %w", path, err)
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		done = true
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	done = true
	return nil
}
