package up

import (
	"context"
	"fmt"
	"time"

	"github.com/okteto/okteto/cmd/utils"
	"github.com/okteto/okteto/pkg/analytics"
	"github.com/okteto/okteto/pkg/config"
	"github.com/okteto/okteto/pkg/errors"
	"github.com/okteto/okteto/pkg/k8s/pods"
	"github.com/okteto/okteto/pkg/log"
	"github.com/okteto/okteto/pkg/syncthing"
)

func (up *upContext) initializeSyncthing() error {
	sy, err := syncthing.New(up.Dev)
	if err != nil {
		return err
	}

	up.Sy = sy

	log.Infof("local syncthing initialized: gui -> %d, sync -> %d", up.Sy.LocalGUIPort, up.Sy.LocalPort)
	log.Infof("remote syncthing initialized: gui -> %d, sync -> %d", up.Sy.RemoteGUIPort, up.Sy.RemotePort)

	if err := up.Sy.SaveConfig(up.Dev); err != nil {
		log.Infof("error saving syncthing object: %s", err)
	}

	up.hardTerminate <- up.Sy.HardTerminate()

	return nil
}

func (up *upContext) sync(ctx context.Context) error {
	if err := up.startSyncthing(ctx); err != nil {
		return err
	}

	return up.synchronizeFiles(ctx)
}

func (up *upContext) startSyncthing(ctx context.Context) error {
	spinner := utils.NewSpinner("Starting the file synchronization service...")
	spinner.Start()
	if err := config.UpdateStateFile(up.Dev, config.StartingSync); err != nil {
		return err
	}
	defer spinner.Stop()

	if err := up.Sy.Run(ctx); err != nil {
		return err
	}

	if err := up.Sy.WaitForPing(ctx, true); err != nil {
		return err
	}

	if err := up.Sy.WaitForPing(ctx, false); err != nil {
		log.Infof("failed to ping syncthing: %s", err.Error())
		err = up.checkOktetoStartError(ctx, "Failed to connect to the synchronization service")
		if err == errors.ErrLostSyncthing {
			if err := pods.Destroy(ctx, up.Pod.Name, up.Dev.Namespace, up.Client); err != nil {
				return fmt.Errorf("error recreating development container: %s", err.Error())
			}
		}
		return err
	}

	if up.resetSyncthing {
		spinner.Update("Resetting synchronization service database...")
		if err := up.Sy.ResetDatabase(ctx, up.Dev, false); err != nil {
			return err
		}
		if err := up.Sy.ResetDatabase(ctx, up.Dev, true); err != nil {
			return err
		}

		if err := up.Sy.WaitForPing(ctx, false); err != nil {
			return err
		}
		if err := up.Sy.WaitForPing(ctx, true); err != nil {
			return err
		}

		up.resetSyncthing = false
	}

	up.Sy.SendStignoreFile(ctx)
	spinner.Update("Scanning file system...")
	if err := up.Sy.WaitForScanning(ctx, up.Dev, true); err != nil {
		return err
	}

	if !up.Dev.PersistentVolumeEnabled() {
		if err := up.Sy.WaitForScanning(ctx, up.Dev, false); err != nil {
			log.Infof("failed to wait for syncthing scanning: %s", err.Error())
			return up.checkOktetoStartError(ctx, "Failed to connect to the synchronization service")
		}
	}

	return nil
}

func (up *upContext) synchronizeFiles(ctx context.Context) error {
	suffix := "Synchronizing your files..."
	spinner := utils.NewSpinner(suffix)
	pbScaling := 0.30

	if err := config.UpdateStateFile(up.Dev, config.Synchronizing); err != nil {
		return err
	}
	spinner.Start()
	defer spinner.Stop()
	reporter := make(chan float64)
	go func() {
		<-time.NewTicker(2 * time.Second).C
		var previous float64

		for c := range reporter {
			if c > previous {
				// todo: how to calculate how many characters can the line fit?
				pb := utils.RenderProgressBar(suffix, c, pbScaling)
				spinner.Update(pb)
				previous = c
			}
		}
	}()

	if err := up.Sy.WaitForCompletion(ctx, up.Dev, reporter); err != nil {
		analytics.TrackSyncError()
		switch err {
		case errors.ErrLostSyncthing:
			return err
		case errors.ErrInsufficientSpace:
			return up.getInsufficientSpaceError(err)
		default:
			return errors.UserError{
				E: err,
				Hint: `Help us improve okteto by filing an issue in https://github.com/okteto/okteto/issues/new.
    Please include the file generated by 'okteto doctor' if possible.
    Then, try to run 'okteto down -v' + 'okteto up' again`,
			}
		}
	}

	// render to 100
	spinner.Update(utils.RenderProgressBar(suffix, 100, pbScaling))

	up.Sy.Type = "sendreceive"
	up.Sy.IgnoreDelete = false
	if err := up.Sy.UpdateConfig(); err != nil {
		return err
	}

	go up.Sy.Monitor(ctx, up.Disconnect)
	go up.Sy.MonitorStatus(ctx, up.Disconnect)
	log.Infof("restarting syncthing to update sync mode to sendreceive")
	return up.Sy.Restart(ctx)
}