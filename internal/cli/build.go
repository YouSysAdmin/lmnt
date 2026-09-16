package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yousysadmin/lmnt/internal/provider/docker"
	"github.com/yousysadmin/lmnt/internal/provider/qemu"
)

func (a *app) buildCommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the guest image for the selected provider",
		Long: `build prepares the Linux guest once, so later commands start quickly.

With --provider qemu it downloads the Alpine Linux installer, installs it
into a disk image in the data directory and adds the tools lmnt needs.
With --provider docker it builds the guest container image.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.report(a.build(cmd.Context(), force))
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "rebuild even if an image exists")

	return cmd
}

func (a *app) build(ctx context.Context, force bool) error {
	if a.opts.provider == providerDocker {
		err := docker.EnsureImage(ctx, a.logger.With("component", "docker"), docker.BuildOptions{Force: force, Debug: a.opts.debug})
		if err != nil {
			return fmt.Errorf("build container image: %w", err)
		}

		a.logger.Info("Guest container image is ready")

		return nil
	}

	dir, err := a.openDataDir()
	if err != nil {
		return err
	}

	out := dir.VMImagePath()

	exists, err := dir.VMImageExists()
	if err != nil {
		return err
	}

	switch {
	case exists && !force:
		a.logger.Info("Guest image already exists, use --force to rebuild", "path", out)
		return nil
	case exists:
		err = dir.Remove(out)
		if err != nil {
			return fmt.Errorf("remove old image: %w", err)
		}
	}

	iso, err := dir.EnsureAlpineISO(ctx)
	if err != nil {
		return fmt.Errorf("alpine installer: %w", err)
	}

	firmware, err := dir.EnsureFirmware(ctx)
	if err != nil {
		return fmt.Errorf("firmware: %w", err)
	}

	a.logger.Info("Building the guest image, this takes a minute or two", "path", out)

	err = qemu.BuildImage(ctx, a.logger.With("component", "qemu"), qemu.BuildConfig{
		ISO:      iso,
		Firmware: firmware,
		Output:   out,
		Debug:    a.opts.debug,
	})
	if err != nil {
		return fmt.Errorf("build guest image: %w", err)
	}

	if err := dir.Remove(iso); err != nil {
		a.logger.Warn("Could not remove the installer ISO", "path", iso, "error", err)
	}

	a.logger.Info("Guest image built", "path", out)

	return nil
}
