package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleContainerTools/kaniko/pkg/config"
	"github.com/GoogleContainerTools/kaniko/pkg/executor"
	"github.com/GoogleContainerTools/kaniko/pkg/util"
	"github.com/containerd/containerd/platforms"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/moby/sys/mount"
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
		return fmt.Errorf("get image config: %w", err)
	}

	out, err := json.MarshalIndent(configFile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image config: %w", err)
	}

	err = os.WriteFile(ImageConfigOutput, out, 0666)
	if err != nil {
		return fmt.Errorf("write image config: %w", err)
	}

	return nil
}

func (cmd *BuildCmd) build() (v1.Image, error) {

	// add ignore paths
	buildIgnorePaths(cmd.IgnorePaths)

	// make sure we detect the correct ignore list
	err := util.InitIgnoreList(true)
	if err != nil {
		return nil, fmt.Errorf("init ignore list: %w", err)
	}

	// create a new directory for the chroot environment
	chrootDir := "/chroot"
	err = os.MkdirAll(chrootDir, 0755)
	if err != nil {
		return nil, fmt.Errorf("create chroot directory: %w", err)
	}

	// copy necessary files and directories into the chroot environment
	err = copyIgnoredFilesToChroot(chrootDir)
	if err != nil {
		return nil, fmt.Errorf("copy files to chroot: %w", err)
	}

	// change root to the chroot environment
	originalRoot, err := os.Open("/")
	if err != nil {
		return nil, fmt.Errorf("get root directory: %w", err)
	}
	chroot, err := os.Open(chrootDir)
	if err != nil {
		originalRoot.Close()
		return nil, fmt.Errorf("open chroot directory: %w", err)
	}
	err = chroot.Chdir()
	if err != nil {
		originalRoot.Close()
		chroot.Close()
		return nil, fmt.Errorf("chdir to chroot: %w", err)
	}
	err = syscall.Chroot(chrootDir)
	if err != nil {
		originalRoot.Close()
		return nil, fmt.Errorf("chroot: %w", err)
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
			return nil, fmt.Errorf("build and add passwd error occurred: %w --- %w", err, addPwdErr)
		}

		return nil, fmt.Errorf("build error: %w", err)
	}

	return image, nil
}

func addPasswd() error {
	err := os.WriteFile("/etc/passwd", []byte("root:x:0:0:root:/root:/.dockerless/bin/sh"), 0666)
	if err != nil {
		return fmt.Errorf("write passwd: %w", err)
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
	util.DeleteFilesystem()
	copyFileOrDir(chrootDir, "/")
	return nil
}

// Copy ignored paths to the chroot environment
func copyIgnoredFilesToChroot(chrootDir string) error {
	effectiveIgnoreList := util.IgnoreList()

	for _, ignore := range effectiveIgnoreList {
		ignorePath := ignore.Path
		ignorePrefixMatchOnly := ignore.PrefixMatchOnly
		if ignorePrefixMatchOnly {
			err := filepath.Walk(ignorePath, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return fmt.Errorf("walk error: %w", err)
				}
				relPath, err := filepath.Rel(ignorePath, path)
				if err != nil {
					return fmt.Errorf("rel error: %w", err)
				}
				destPath := filepath.Join(chrootDir, ignorePath, relPath)
				if info.IsDir() {
					if err := os.MkdirAll(destPath, info.Mode()); err != nil {
						return fmt.Errorf("mkdir error: %w", err)
					}
					return nil
				}
				return copyFile(path, destPath)
			})
			if err != nil {
				return fmt.Errorf("walk error: %w", err)
			}
		} else {
			if err := copyFileOrDir(ignorePath, filepath.Join(chrootDir, ignorePath)); err != nil {
				return fmt.Errorf("copy error: %w", err)
			}
		}
	}

	return nil
}

// copyFileOrDir copies a file or directory from src to dst
func copyFileOrDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat src: %w", err)
	}

	if srcInfo.IsDir() {
		return copyDir(src, dst)
	}
	return copyFile(src, dst)
}

// copyFile copies a single file from src to dst
func copyFile(src, dst string) error {
	// Ensure the parent directory exists
	dstDir := filepath.Dir(dst)
	err := os.MkdirAll(dstDir, 0755)
	if err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src file: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst file: %w", err)
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return fmt.Errorf("copy file: %w", err)
	}

	err = dstFile.Sync()
	if err != nil {
		return fmt.Errorf("sync dst file: %w", err)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat src file: %w", err)
	}

	err = os.Chmod(dst, srcInfo.Mode())
	if err != nil {
		return fmt.Errorf("chmod dst file: %w", err)
	}

	return nil
}

// copyDir copies a directory recursively from src to dst
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat src dir: %w", err)
	}

	err = os.MkdirAll(dst, srcInfo.Mode())
	if err != nil {
		return fmt.Errorf("mkdir dst dir: %w", err)
	}

	// Check if the directory is a mount point
	for _, volume := range util.Volumes() {
		if strings.HasPrefix(src, volume) {
			// Remount the volume instead of copying
			target := filepath.Join(dst, src)
			err := os.MkdirAll(target, 0755)
			if err != nil {
				return fmt.Errorf("mkdir error: %w", err)
			}
			err = mount.Mount(src, target, "bind", "")
			if err != nil {
				return fmt.Errorf("mount error: %w", err)
			}
			return nil
		}
	}

	// Copy the directory
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read src dir: %w", err)
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		// Prevent copying the directory into itself
		if srcPath == dstPath {
			continue
		}

		if entry.IsDir() {
			err = copyDir(srcPath, dstPath)
			if err != nil {
				return fmt.Errorf("copy dir: %w", err)
			}
		} else {
			err = copyFile(srcPath, dstPath)
			if err != nil {
				return fmt.Errorf("copy file: %w", err)
			}
		}
	}

	return nil
}
