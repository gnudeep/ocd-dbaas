package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/dbaas/api/v1alpha1"
	"github.com/wso2/open-cloud-datacenter/dbaas/internal/harvester"
)

// DBSnapshotReconciler reconciles DBSnapshot CRDs.
// Each snapshot is a one-shot operation: the reconciler SSHes into the VM,
// triggers a pgBackRest backup, and transitions the snapshot to available or failed.
// Once in a terminal phase it never requeues.
type DBSnapshotReconciler struct {
	client.Client
	Harvester *harvester.Client
}

func (r *DBSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbaasv1.DBSnapshot{}).
		Complete(r)
}

func (r *DBSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var snap dbaasv1.DBSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Already in a terminal phase — nothing to do.
	if snap.Status.Phase == "available" || snap.Status.Phase == "failed" {
		return ctrl.Result{}, nil
	}

	// Fetch the parent DBInstance.
	var inst dbaasv1.DBInstance
	instKey := client.ObjectKey{Namespace: snap.Namespace, Name: snap.Spec.DBInstanceRef}
	if err := r.Get(ctx, instKey, &inst); err != nil {
		if errors.IsNotFound(err) {
			return r.fail(ctx, &snap, fmt.Sprintf("DBInstance %q not found", snap.Spec.DBInstanceRef))
		}
		return ctrl.Result{}, err
	}

	// Guard: DBInstance must be Available before we try to SSH in.
	if inst.Status.Phase != dbaasv1.StatusAvailable {
		logger.Info("DBInstance not yet available, requeuing", "phase", inst.Status.Phase)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	vmIP := inst.Status.ManagementAddress
	if vmIP == "" {
		logger.Info("management address not yet assigned, requeuing")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Mark as in-progress.
	snap.Status.Phase = "creating"
	if err := r.Status().Update(ctx, &snap); err != nil {
		return ctrl.Result{}, err
	}

	// Read the credentials Secret to get the SSH private key.
	secretData, err := r.Harvester.GetSecret(ctx, snap.Namespace, inst.Status.Resources.SecretName)
	if err != nil {
		return r.fail(ctx, &snap, fmt.Sprintf("read credentials secret: %v", err))
	}
	sshKey := string(secretData[dbaasv1.SecretKeySSHPrivateKey])
	if sshKey == "" {
		return r.fail(ctx, &snap, "credentials secret missing ssh_private_key — was this instance provisioned before backup support was added?")
	}

	// Determine backup type (default to incremental if not specified).
	backupType := snap.Spec.SnapshotType
	if backupType == "" {
		backupType = "incr"
	}

	// SSH into the VM and trigger the pgBackRest backup.
	// pgBackRest runs as the postgres OS user; sudo is required because ubuntu
	// does not have direct access to postgres's pgbackrest config.
	cmd := fmt.Sprintf("sudo -u postgres pgbackrest --stanza=%s backup --type=%s",
		inst.Name, backupType)
	logger.Info("triggering pgBackRest backup", "vm", vmIP, "type", backupType, "stanza", inst.Name)

	out, sshErr := r.Harvester.SSHExec(ctx, vmIP, sshKey, cmd)
	if sshErr != nil {
		msg := fmt.Sprintf("%v\n\npgbackrest output:\n%s", sshErr, out)
		return r.fail(ctx, &snap, msg)
	}

	// Success — record the S3 path and mark available.
	now := metav1.Now()
	snap.Status.Phase = "available"
	snap.Status.Done = &now
	snap.Status.S3Path = fmt.Sprintf("/%s/backup/%s", inst.Name, backupType)
	snap.Status.Message = out
	if err := r.Status().Update(ctx, &snap); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("backup completed", "snapshot", snap.Name, "s3Path", snap.Status.S3Path)
	return ctrl.Result{}, nil
}

// fail sets the snapshot phase to failed with a message and stops requeuing.
func (r *DBSnapshotReconciler) fail(ctx context.Context, snap *dbaasv1.DBSnapshot, msg string) (ctrl.Result, error) {
	snap.Status.Phase = "failed"
	snap.Status.Message = msg
	_ = r.Status().Update(ctx, snap)
	return ctrl.Result{}, nil
}
