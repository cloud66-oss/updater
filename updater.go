package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/mitchellh/ioprogress"
)

// Updater is responsible for checking for updates and updating the running executable
type Updater struct {
	CurrentVersion string

	currentVersion *version.Version
	options        *Options
}

// NewUpdater returns a new updater instance
func NewUpdater(currentVersion string, options *Options) (*Updater, error) {
	if options.RemoteURL == "" {
		panic("no RemoteURL")
	}
	if !strings.HasSuffix(options.RemoteURL, "/") {
		options.RemoteURL = options.RemoteURL + "/"
	}
	if options.VersionSpecsFilename == "" {
		options.VersionSpecsFilename = "versions.json"
	}
	if options.Channel == "" {
		options.Channel = "dev"
	}
	if options.BinPattern == "" {
		options.BinPattern = "{{OS}}_{{ARCH}}_{{VERSION}}"
	}

	v, err := version.NewVersion(currentVersion)
	if err != nil {
		return nil, err
	}

	return &Updater{
		currentVersion: v,
		options:        options,
	}, nil
}

// Run runs the updater
func (u *Updater) Run(force bool) error {
	_, _, err := u.RunWithOutcome(force)
	return err
}

// RunWithOutcome runs the updater, returns whether an update was performed and debug lines if debug is enabled
func (u *Updater) RunWithOutcome(force bool) (bool, []string, error) {
	var debugLines []string

	remoteVersion, err := u.getRemoteVersion()
	if err != nil {
		return false, debugLines, err
	}

	rVersion, err := version.NewVersion(remoteVersion.Version)
	if err != nil {
		return false, debugLines, fmt.Errorf("remote version is '%s'. %s", remoteVersion.Version, err)
	}

	if u.options.Debug {
		debugLines = append(debugLines, fmt.Sprintf("Local Version %v - Remote Version: %v", u.currentVersion, rVersion))
	}
	if !u.options.Silent {
		fmt.Printf("Local Version %v - Remote Version: %v\n", u.currentVersion, rVersion)
	}

	// check if update needed
	if force || remoteVersion.Force || u.currentVersion.LessThan(rVersion) {
		debugLines, err = u.downloadAndReplace(rVersion, debugLines)
		if err != nil {
			// error during update
			return false, debugLines, err
		}
		// successfully updated
		return true, debugLines, nil
	}

	// no update needed
	if u.options.Debug {
		debugLines = append(debugLines, "No update needed")
	}
	return false, debugLines, nil
}

func (u *Updater) downloadAndReplace(remoteVersion *version.Version, debugLines []string) ([]string, error) {
	// fetch the new file
	binURL := generateURL(u.options.BinURL(), remoteVersion.String())
	err := remoteFileExists(binURL)
	if err != nil {
		return debugLines, err
	}

	bodyResp, err := http.Get(binURL)
	if err != nil {
		return debugLines, err
	}
	defer bodyResp.Body.Close()

	var data []byte
	if !u.options.Silent {
		progressR := &ioprogress.Reader{
			Reader:       bodyResp.Body,
			Size:         bodyResp.ContentLength,
			DrawInterval: 500 * time.Millisecond,
			DrawFunc: ioprogress.DrawTerminalf(os.Stdout, func(progress, total int64) string {
				bar := ioprogress.DrawTextFormatBar(40)
				return fmt.Sprintf("%s %20s", bar(progress, total), ioprogress.DrawTextFormatBytes(progress, total))
			}),
		}
		data, err = io.ReadAll(progressR)
		if err != nil {
			return debugLines, err
		}
	} else {
		data, err = io.ReadAll(bodyResp.Body)
		if err != nil {
			return debugLines, err
		}
	}

	dest, err := os.Executable()
	if err != nil {
		return debugLines, err
	}

	if u.options.Debug {
		debugLines = append(debugLines, fmt.Sprintf("current executable is %s", dest))
	}

	if !u.options.Silent {
		fmt.Printf("Downloading the new version to %s\n", dest)
	}

	// get the original file's permissions
	originalFileInfo, err := os.Stat(dest)
	if err != nil {
		return debugLines, err
	}
	originalMode := originalFileInfo.Mode()
	if u.options.Debug {
		debugLines = append(debugLines, fmt.Sprintf("original file mode is %s", originalMode))
	}

	// create a temporary file in the same directory as the destination
	destName := filepath.Base(dest)
	tmpFile, err := os.CreateTemp(filepath.Dir(dest), destName+".download.")
	if err != nil {
		return debugLines, err
	}
	tmpPath := tmpFile.Name()
	if u.options.Debug {
		debugLines = append(debugLines, fmt.Sprintf("temporary file is %s", tmpPath))
	}
	// set permissions on the temporary file
	if err := os.Chmod(tmpPath, originalMode); err != nil {
		os.Remove(tmpPath)
		return debugLines, err
	}
	// write to the temporary file
	if err := os.WriteFile(tmpPath, data, originalMode); err != nil {
		os.Remove(tmpPath)
		return debugLines, err
	}

	// close the temporary file before renaming
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return debugLines, err
	}
	if u.options.Debug {
		debugLines = append(debugLines, fmt.Sprintf("wrote to temporary file %s", tmpPath))
	}
	// atomic rename to final destination
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return debugLines, err
	}
	if u.options.Debug {
		debugLines = append(debugLines, fmt.Sprintf("renamed temporary file %s to %s", tmpPath, dest))
	}
	return debugLines, nil
}

func (u *Updater) getRemoteVersion() (*VersionSpec, error) {
	err := remoteFileExists(u.options.VersionSpecsURL())
	if err != nil {
		return nil, err
	}

	response, err := http.Get(u.options.VersionSpecsURL())
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, errors.New("invalid version specification file")
	}

	b, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	// get the version descriptor
	var versions VersionSpecs
	err = json.Unmarshal(b, &versions)
	if err != nil {
		return nil, err
	}

	return versions.GetVersionByChannel(u.options.Channel)
}

func generateURL(path string, version string) string {
	path = strings.Replace(path, "{{OS}}", runtime.GOOS, -1)
	path = strings.Replace(path, "{{ARCH}}", runtime.GOARCH, -1)
	path = strings.Replace(path, "{{VERSION}}", version, -1)

	return path
}

func remoteFileExists(path string) error {
	resp, err := http.Head(path)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote file not found at %s", path)
	}

	return nil

}
