package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/GoogleContainerTools/kaniko/pkg/config"
	"github.com/GoogleContainerTools/kaniko/pkg/executor"
	"github.com/GoogleContainerTools/kaniko/pkg/util"
	"github.com/containerd/containerd/platforms"
	"github.com/gokrazy/rsync/rsynccmd"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"
)

const defaultCacheDir = "/.dockerless/cache"

var ImageConfigOutput = "/.dockerless/image.json"

type BuildCmd struct {
	Dockerfile    string
	Context       string
	Target        string
	RegistryCache string
	BuildArgs     []string
	IgnorePaths   []string
	Insecure      bool
	ExportCache   bool
}

// NewBuildCmd returns a new build command
func NewBuildCmd() *cobra.Command {
	cmd := &BuildCmd{}
	cobraCmd := &cobra.Command{
		Use:           "build",
		Short:         "Build the container",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return cmd.Run()
		},
	}

	cobraCmd.Flags().StringVar(&cmd.Dockerfile, "dockerfile", "", "Dockerfile to build from.")
	cobraCmd.Flags().StringVar(&cmd.Target, "target", "", "The docker target stage to build.")
	cobraCmd.Flags().StringVar(&cmd.Context, "context", "", "Context to build from.")
	cobraCmd.Flags().StringArrayVar(&cmd.BuildArgs, "build-arg", []string{}, "Docker build args.")
	cobraCmd.Flags().StringArrayVar(&cmd.IgnorePaths, "ignore-path", []string{}, "Extra paths to exclude from deletion.")
	cobraCmd.Flags().BoolVar(&cmd.Insecure, "insecure", true, "If true will not check for certificates")
	cobraCmd.Flags().StringVar(&cmd.RegistryCache, "registry-cache", "", "Registry to use as remote cache.")
	cobraCmd.Flags().BoolVar(&cmd.ExportCache, "export-cache", false, "If true kanoiko build push cache to registry.")
	return cobraCmd
}

func (cmd *BuildCmd) Run() error {
	// check if we already have built the image
	_, err := os.Stat(ImageConfigOutput)
	if err == nil {
		fmt.Println("skip building, because image is already built")
		return nil
	}

	// fill parameters through env
	if cmd.Dockerfile == "" {
		cmd.Dockerfile = os.Getenv("DOCKERLESS_DOCKERFILE")
		if cmd.Dockerfile == "" {
			return fmt.Errorf("--dockerfile is missing")
		}
	}
	if cmd.Context == "" {
		cmd.Context = os.Getenv("DOCKERLESS_CONTEXT")
		if cmd.Context == "" {
			return fmt.Errorf("--context is missing")
		}
	}
	if cmd.Target == "" {
		cmd.Target = os.Getenv("DOCKERLESS_TARGET")
	}

	// parse extra build args
	buildArgs := os.Getenv("DOCKERLESS_BUILD_ARGS")
	if buildArgs != "" {
		extraBuildArgs := []string{}
		_ = json.Unmarshal([]byte(buildArgs), &extraBuildArgs)
		cmd.BuildArgs = append(cmd.BuildArgs, extraBuildArgs...)
	}

	// start actual build
	image, err := cmd.build()
	if err != nil {
		return err
	}

	// write config file to file
	configFile, err := image.ConfigFile()
	if err != nil {
		return fmt.Errorf("get image config <- %w", err)
	}

	out, err := json.MarshalIndent(configFile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image config <- %w", err)
	}

	err = os.WriteFile(ImageConfigOutput, out, 0666)
	if err != nil {
		return fmt.Errorf("write image config <- %w", err)
	}

	return nil
}

func (cmd *BuildCmd) build() (v1.Image, error) {

	// add ignore paths
	buildIgnorePaths(cmd.IgnorePaths)

	// make sure we detect the correct ignore list
	err := util.InitIgnoreList(true)
	if err != nil {
		return nil, fmt.Errorf("init ignore list <- %w", err)
	}

	// create a new directory for the chroot environment
	chrootDir := "/chroot"
	err = os.MkdirAll(chrootDir, 0755)
	if err != nil {
		return nil, fmt.Errorf("create chroot directory <- %w", err)
	}

	// copy necessary files and directories into the chroot environment
	err = copyIgnoredFilesToChroot(chrootDir)
	if err != nil {
		return nil, fmt.Errorf("copy files to chroot <- %w", err)
	}

	// change root to the chroot environment
	originalRoot, err := os.Open("/")
	if err != nil {
		return nil, fmt.Errorf("get root directory <- %w", err)
	}
	chroot, err := os.Open(chrootDir)
	if err != nil {
		originalRoot.Close()
		return nil, fmt.Errorf("open chroot directory <- %w", err)
	}
	err = chroot.Chdir()
	if err != nil {
		originalRoot.Close()
		chroot.Close()
		return nil, fmt.Errorf("chdir to chroot <- %w", err)
	}
	err = syscall.Chroot(chrootDir)
	if err != nil {
		originalRoot.Close()
		return nil, fmt.Errorf("chroot <- %w", err)
	}
	defer func() {
		defer originalRoot.Close()
		// change to the original root directory
		if err := originalRoot.Chdir(); err != nil {
			fmt.Printf("chroot back to original: %v\n", err)
		}
		// restore the original root directory
		syscall.Chroot(".")
	}()

	opts := &config.KanikoOptions{
		Destinations:   []string{"local"},
		Unpack:         true,
		BuildArgs:      cmd.BuildArgs,
		DockerfilePath: cmd.Dockerfile,
		RegistryOptions: config.RegistryOptions{
			Insecure:      cmd.Insecure,
			InsecurePull:  cmd.Insecure,
			SkipTLSVerify: cmd.Insecure,
		},
		SrcContext:          cmd.Context,
		Target:              cmd.Target,
		CustomPlatform:      platforms.Format(platforms.Normalize(platforms.DefaultSpec())),
		SnapshotMode:        "redo",
		RunV2:               true,
		NoPush:              true,
		KanikoDir:           "/.dockerless",
		Cache:               true,
		CacheRunLayers:      true,
		CacheCopyLayers:     true,
		CompressedCaching:   true,
		SkipUnusedStages:    true,
		ImageFSExtractRetry: 3,
		NoPushCache:         !cmd.ExportCache,
		Compression:         config.ZStd,
		CompressionLevel:    3,
		CacheOptions: config.CacheOptions{
			CacheTTL: time.Hour * 24 * 7,
		},
	}
	if cmd.RegistryCache != "" {
		opts.CacheRepo = cmd.RegistryCache
	} else {
		opts.CacheOptions.CacheDir = defaultCacheDir
	}
	if !cmd.ExportCache {
		opts.SingleSnapshot = true
	}

	// let's build!
	image, err := executor.DoBuild(opts)

	if err != nil {
		// add a passwd as other we won't be able to exec into this container
		if addPwdErr := addPasswd(); addPwdErr != nil {
			return nil, fmt.Errorf("build and add passwd error occurred <- %w --- %w", err, addPwdErr)
		}

		return nil, fmt.Errorf("build error <- %w", err)
	}

	return image, nil
}

func addPasswd() error {
	err := os.WriteFile("/etc/passwd", []byte("root:x:0:0:root:/root:/.dockerless/bin/sh"), 0666)
	if err != nil {
		return fmt.Errorf("write passwd <- %w", err)
	}

	return nil
}

func buildIgnorePaths(extraPaths []string) {
	// we need to add a couple of extra ignore paths for kaniko
	ignorePaths := append([]string{
		"/.dockerless",
		"/workspaces",
		"/etc/envfile.json",
		"/etc/resolv.conf",
		"/var/run",
		"/product_uuid",
	}, extraPaths...)
	for _, ignorePath := range ignorePaths {
		util.AddToDefaultIgnoreList(util.IgnoreListEntry{
			Path:            ignorePath,
			PrefixMatchOnly: false,
		})
	}
}

// OverrideRoot override the root directory to the chrootDir
func overrideRoot(chrootDir string) error {
	args := []string{"--delete", chrootDir + "/", "root@localhost:/"}

	return rsync(args...)
}

// Copy ignored paths to the chroot environment
func copyIgnoredFilesToChroot(chrootDir string) error {
	effectiveIgnoreList := util.IgnoreList()
	includes := []string{}

	for _, ignore := range effectiveIgnoreList {
		includes = append(includes, ignore.Path)
	}

	err := rsyncCopyWithInclude("/", chrootDir, includes)
	if err != nil {
		return fmt.Errorf("rsync copy with include <- %w", err)
	}

	return nil
}

func rsyncCopyWithInclude(src, dst string, includes []string) error {

	// Create a temporary directory to hold the filtered files
	tempDir, err := os.MkdirTemp("", "rsync-include")
	if err != nil {
		return fmt.Errorf("create temp directory <- %w", err)
	}
	defer os.RemoveAll(tempDir)

	// Copy the included files and directories to the temporary directory
	for _, include := range includes {
		srcPath := filepath.Join(src, include)
		dstPath := filepath.Join(tempDir, include)

		err := rsyncFileOrDir(srcPath, filepath.Dir(dstPath))
		if err != nil {
			return fmt.Errorf("rsync file or directory <- %w", err)
		}
	}

	// Use rsync to copy the filtered files and directories to the destination
	args := []string{tempDir + "/", "root@localhost:" + dst}
	return rsync(args...)
}

// copyFileOrDir copies a file or directory from src to dst
func rsyncFileOrDir(src, dstDir string) error {
	_, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat src <- %w", err)
	}
	args := []string{src, "root@localhost:" + dstDir + "/"}
	return rsync(args...)
}

func rsync(args ...string) error {
	ctx := context.Background()
	args = append([]string{"-av", "--progress"}, args...)
	cmd := rsynccmd.Command("rsync", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	_, err := cmd.Run(ctx)
	return err
}
