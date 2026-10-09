package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/MobAI-App/ios-builder/internal/config"
	"github.com/MobAI-App/ios-builder/internal/runner"
	"github.com/spf13/cobra"
)

var centralDetectCmd = &cobra.Command{
	Use:   "detect",
	Short: "Show what `central setup` and `central register` would read from this project",
	Long: `Prints the app folder, iOS folder, framework, bundle identifier and pinned
tool versions found in the project, without touching the network, builder.json
or the registry. The same detection feeds setup, register and the runner.`,
	Args: cobra.NoArgs,
	RunE: runCentralDetect,
}

func init() {
	centralCmd.AddCommand(centralDetectCmd)
	centralDetectCmd.Flags().String("dir", ".", "Repository folder to inspect")
	centralDetectCmd.Flags().String("app-path", "", "Folder holding the app when the repository has several")
	centralDetectCmd.Flags().String("bundle-id", "", "Bundle identifier override")
}

func runCentralDetect(cmd *cobra.Command, _ []string) error {
	dir, _ := cmd.Flags().GetString("dir")
	appPath, _ := cmd.Flags().GetString("app-path")
	bundleFlag, _ := cmd.Flags().GetString("bundle-id")
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	cfg := &config.Config{IOS: config.IOSConfig{AppPath: appPath}}
	facts, err := readProjectFacts(root, cfg, bundleFlag)
	if err != nil {
		return err
	}
	appRoot := filepath.Join(root, filepath.FromSlash(facts.AppPath))
	versions := runner.ReadToolVersions(root, appRoot)
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "app_path=%s\nios_path=%s\nframework=%s\nbundle_id=%s\n", facts.AppPath, facts.IOSPath, facts.Framework, facts.BundleID)
	fmt.Fprintf(out, "flutter_version=%s\nnode_version=%s\nxcode_version=%s\ngodot_version=%s\n", versions.Flutter, versions.Node, versions.Xcode, versions.Godot)
	for _, note := range facts.Notes {
		fmt.Fprintln(os.Stderr, note)
	}
	return nil
}
