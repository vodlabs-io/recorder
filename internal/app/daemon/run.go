package daemon

import (
	"context"
	"log"
	"time"
)

func (a *App) Run(ctx context.Context) error {
	defer a.cleanup()

	// serviceCtx controls the session registry, cloud client, and other
	// background services. Cancelled by OS signal (via ctx) or explicitly
	// on a CLI shutdown request so Sessions wrap up either way.
	serviceCtx, serviceCancel := context.WithCancel(ctx)
	defer serviceCancel()

	if err := a.start(serviceCtx); err != nil {
		return err
	}

	// Wait for shutdown condition
	select {
	case <-ctx.Done():
		a.log.Info("Received shutdown signal")
		a.procSvc.CancelProcessing() // OS signal: abort pending processing
	case <-a.apiSvc.ShutdownCh:
		a.log.Info("Shutting down via NATS request")
		serviceCancel() // Stop recording sessions; processing keeps draining.
	case err := <-a.errCh:
		a.log.Error("Shutting down due to fatal error", "error", err)
		serviceCancel() // Stop recording sessions; processing keeps draining.
	}

	a.sessionReg.Wait()                // Wait for all session workers to finish
	close(a.procSvc.PendingRecordings) // Signal processing worker to finish

	select {
	case <-a.procSvc.WorkerDone: // Processing worker already finished
	case <-time.After(1 * time.Millisecond): // Avoids spurious log when processing worker exits with no jobs
		a.log.Info("Processing pending recordings. Send 'shutdown_force' to cancel.")
		log.Default().Print("Processing pending recordings...")
		select {
		case <-a.procSvc.WorkerDone:
		case <-a.apiSvc.ForceShutdownCh:
			a.log.Info("Force shutdown requested. Cancelling processing.")
			a.procSvc.CancelProcessing()
			<-a.procSvc.WorkerDone
		case <-ctx.Done():
			a.log.Info("Received shutdown signal during processing. Cancelling.")
			a.procSvc.CancelProcessing()
			<-a.procSvc.WorkerDone
		}
	}
	return nil
}
