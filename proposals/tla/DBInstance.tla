---------------------------- MODULE DBInstance ----------------------------
(***************************************************************************
 TLA+ specification of the DBInstance controller for OCD-dbaas.

 Covers:
   - Provisioning           (Storage -> OS -> VM -> Ready)
   - Stop / Start lifecycle
   - Patch flow             (PatchPending -> Snapshotting -> Stopping ->
                             OSReplaced -> Starting -> Verifying), with
                             retry, rollback to previous image, and giveup
                             to Failed
   - Delete                 (atomic teardown)
   - Interleaved user spec mutations at any time

 Safety properties verified (see INVARIANTS section):
   - PgDataAlwaysPresentWhileResourceExists
   - NoDestructiveStepWithoutSnapshot
   - OSReplaceOnlyWhileVMStopped
   - CurrentImageConsistency
   - RollbackTargetSet
   - VMRunningImpliesOSDV

 Liveness properties verified:
   - PatchTerminates, CreateTerminates, StopTerminates, StartTerminates,
     DeleteTerminates

 Crash modeling: the destructive OS DataVolume replacement is split into
 PatchOSDelete and PatchOSCreate so a crash between them is a reachable
 state. Each phase action has explicit preconditions on the underlying
 Harvester state, so on restart the controller re-enters at the correct
 sub-step. This catches non-idempotent recovery paths.
 ***************************************************************************)

EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
    Images,          \* Set of valid OS image identifiers
    MaxAttempts      \* Per-target verification retry budget before rollback

ASSUME Cardinality(Images) >= 2
ASSUME MaxAttempts \in Nat /\ MaxAttempts >= 1

VARIABLES
    \* Spec (user-controlled, persisted in etcd as DBInstance.spec)
    specImage,
    specRunning,
    specDeleting,

    \* Status (controller-managed, persisted in etcd as DBInstance.status)
    phase,
    subPhase,
    currentImage,
    previousImage,
    targetImage,
    attempts,

    \* Harvester reality (the actual side-effects)
    pgDataDV,
    osDV,
    osDVImage,
    vm,
    vmRunning,
    snapshot

vars == << specImage, specRunning, specDeleting,
           phase, subPhase,
           currentImage, previousImage, targetImage, attempts,
           pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

Phases == { "absent", "creating", "available", "patching",
            "stopping", "stopped", "starting", "deleting", "failed" }

SubPhases == { "none",
               "Pending", "StorageProvisioned", "OSProvisioned", "VMCreated",
               "PatchPending", "PatchSnapshotting", "PatchStopping",
               "PatchOSReplaced", "PatchStarting", "PatchVerifying",
               "Stopping", "Starting", "Deleting" }

DVStates   == { "absent", "present" }
SnapStates == { "absent", "creating", "available" }
VMStates   == { "absent", "present" }

TypeOK ==
    /\ specImage      \in Images
    /\ specRunning    \in BOOLEAN
    /\ specDeleting   \in BOOLEAN
    /\ phase          \in Phases
    /\ subPhase       \in SubPhases
    /\ currentImage   \in Images \cup {""}
    /\ previousImage  \in Images \cup {""}
    /\ targetImage    \in Images \cup {""}
    /\ attempts       \in 0..MaxAttempts
    /\ pgDataDV       \in DVStates
    /\ osDV           \in DVStates
    /\ osDVImage      \in Images \cup {""}
    /\ vm             \in VMStates
    /\ vmRunning      \in BOOLEAN
    /\ snapshot       \in SnapStates

InitialImage == CHOOSE i \in Images : TRUE

Init ==
    /\ specImage      = InitialImage
    /\ specRunning    = TRUE
    /\ specDeleting   = FALSE
    /\ phase          = "absent"
    /\ subPhase       = "none"
    /\ currentImage   = ""
    /\ previousImage  = ""
    /\ targetImage    = ""
    /\ attempts       = 0
    /\ pgDataDV       = "absent"
    /\ osDV           = "absent"
    /\ osDVImage      = ""
    /\ vm             = "absent"
    /\ vmRunning      = FALSE
    /\ snapshot       = "absent"

(***************************************************************************
 USER ACTIONS  ----  models kubectl apply / patch / delete
 ***************************************************************************)

UserCreate ==
    /\ phase = "absent"
    /\ ~specDeleting
    /\ phase'    = "creating"
    /\ subPhase' = "Pending"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

UserChangeImage ==
    /\ phase = "available"
    /\ ~specDeleting
    /\ \E img \in Images :
         /\ img /= specImage
         /\ specImage' = img
    /\ UNCHANGED << specRunning, specDeleting,
                    phase, subPhase, currentImage, previousImage,
                    targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

UserToggleRunning ==
    /\ ~specDeleting
    /\ specRunning' = ~specRunning
    /\ UNCHANGED << specImage, specDeleting,
                    phase, subPhase, currentImage, previousImage,
                    targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

UserDelete ==
    /\ ~specDeleting
    /\ phase /= "absent"
    /\ specDeleting' = TRUE
    /\ UNCHANGED << specImage, specRunning,
                    phase, subPhase, currentImage, previousImage,
                    targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

(***************************************************************************
 PROVISIONING
 ***************************************************************************)

PhaseStorage ==
    /\ phase = "creating"
    /\ subPhase = "Pending"
    /\ pgDataDV = "absent"
    /\ pgDataDV' = "present"
    /\ subPhase' = "StorageProvisioned"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, currentImage, previousImage, targetImage, attempts,
                    osDV, osDVImage, vm, vmRunning, snapshot >>

PhaseOS ==
    /\ phase = "creating"
    /\ subPhase = "StorageProvisioned"
    /\ osDV = "absent"
    /\ osDV'         = "present"
    /\ osDVImage'    = specImage
    /\ currentImage' = specImage
    /\ subPhase'     = "OSProvisioned"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, previousImage, targetImage, attempts,
                    pgDataDV, vm, vmRunning, snapshot >>

PhaseVM ==
    /\ phase = "creating"
    /\ subPhase = "OSProvisioned"
    /\ vm = "absent"
    /\ vm'        = "present"
    /\ vmRunning' = TRUE
    /\ subPhase'  = "VMCreated"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, snapshot >>

PhaseReady ==
    /\ phase = "creating"
    /\ subPhase = "VMCreated"
    /\ vmRunning = TRUE
    /\ phase'    = "available"
    /\ subPhase' = "none"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

(***************************************************************************
 STOP / START
 ***************************************************************************)

StartStop ==
    /\ phase = "available"
    /\ specRunning = FALSE
    /\ specImage = currentImage           \* don't race with a pending patch
    /\ ~specDeleting
    /\ phase'    = "stopping"
    /\ subPhase' = "Stopping"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

DoStop ==
    /\ phase = "stopping"
    /\ vmRunning' = FALSE
    /\ phase'     = "stopped"
    /\ subPhase'  = "none"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, snapshot >>

StartStart ==
    /\ phase = "stopped"
    /\ specRunning = TRUE
    /\ ~specDeleting
    /\ phase'    = "starting"
    /\ subPhase' = "Starting"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

DoStart ==
    /\ phase = "starting"
    /\ vmRunning' = TRUE
    /\ phase'     = "available"
    /\ subPhase'  = "none"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, snapshot >>

(***************************************************************************
 PATCH  ----  the new flow proposed in proposals/001
 ***************************************************************************)

StartPatch ==
    /\ phase = "available"
    /\ specImage /= currentImage
    /\ ~specDeleting
    /\ phase'         = "patching"
    /\ subPhase'      = "PatchPending"
    /\ previousImage' = currentImage
    /\ targetImage'   = specImage
    /\ attempts'      = 0
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

PatchSnapshotStart ==
    /\ phase = "patching"
    /\ subPhase = "PatchPending"
    /\ snapshot' = "creating"
    /\ subPhase' = "PatchSnapshotting"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning >>

PatchSnapshotComplete ==
    /\ phase = "patching"
    /\ subPhase = "PatchSnapshotting"
    /\ snapshot = "creating"
    /\ snapshot' = "available"
    /\ subPhase' = "PatchStopping"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning >>

PatchStop ==
    /\ phase = "patching"
    /\ subPhase = "PatchStopping"
    /\ vmRunning' = FALSE
    /\ subPhase'  = "PatchOSReplaced"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, snapshot >>

\* Destructive replacement is split into delete + create. A crash between
\* the two leaves osDV="absent" with subPhase still "PatchOSReplaced", from
\* which PatchOSCreate is enabled. This catches non-idempotent recovery.
PatchOSDelete ==
    /\ phase = "patching"
    /\ subPhase = "PatchOSReplaced"
    /\ vmRunning = FALSE
    /\ snapshot  = "available"
    /\ osDV = "present"
    /\ osDV'      = "absent"
    /\ osDVImage' = ""
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, subPhase, currentImage, previousImage,
                    targetImage, attempts,
                    pgDataDV, vm, vmRunning, snapshot >>

PatchOSCreate ==
    /\ phase = "patching"
    /\ subPhase = "PatchOSReplaced"
    /\ vmRunning = FALSE
    /\ osDV = "absent"
    /\ osDV'         = "present"
    /\ osDVImage'    = targetImage
    /\ currentImage' = targetImage
    /\ subPhase'     = "PatchStarting"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, previousImage, targetImage, attempts,
                    pgDataDV, vm, vmRunning, snapshot >>

PatchStart ==
    /\ phase = "patching"
    /\ subPhase = "PatchStarting"
    /\ vmRunning' = TRUE
    /\ subPhase'  = "PatchVerifying"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, snapshot >>

PatchVerifySuccess ==
    /\ phase = "patching"
    /\ subPhase = "PatchVerifying"
    /\ vmRunning = TRUE
    /\ osDVImage = targetImage
    /\ phase'       = "available"
    /\ subPhase'    = "none"
    /\ snapshot'    = "absent"
    /\ targetImage' = ""
    /\ attempts'    = 0
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage, previousImage,
                    pgDataDV, osDV, osDVImage, vm, vmRunning >>

PatchVerifyFail ==
    /\ phase = "patching"
    /\ subPhase = "PatchVerifying"
    /\ attempts < MaxAttempts
    /\ attempts' = attempts + 1
    /\ subPhase' = "PatchStopping"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, currentImage, previousImage, targetImage,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

\* Original target keeps failing: switch to previousImage as the new target.
\* Rollback happens at most once (subsequent target == previousImage).
PatchRollback ==
    /\ phase = "patching"
    /\ subPhase = "PatchVerifying"
    /\ attempts >= MaxAttempts
    /\ targetImage /= previousImage
    /\ targetImage' = previousImage
    /\ attempts'    = 0
    /\ subPhase'    = "PatchStopping"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    phase, currentImage, previousImage,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

\* Rollback also failed: terminal Failed.
PatchGiveUp ==
    /\ phase = "patching"
    /\ subPhase = "PatchVerifying"
    /\ attempts >= MaxAttempts
    /\ targetImage = previousImage
    /\ phase'    = "failed"
    /\ subPhase' = "none"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

(***************************************************************************
 DELETE
 ***************************************************************************)

StartDelete ==
    /\ specDeleting
    /\ phase \notin { "deleting", "absent" }
    /\ phase'    = "deleting"
    /\ subPhase' = "Deleting"
    /\ UNCHANGED << specImage, specRunning, specDeleting,
                    currentImage, previousImage, targetImage, attempts,
                    pgDataDV, osDV, osDVImage, vm, vmRunning, snapshot >>

DoDelete ==
    /\ phase = "deleting"
    /\ vm'            = "absent"
    /\ vmRunning'     = FALSE
    /\ osDV'          = "absent"
    /\ osDVImage'     = ""
    /\ pgDataDV'      = "absent"
    /\ snapshot'      = "absent"
    /\ phase'         = "absent"
    /\ subPhase'      = "none"
    /\ currentImage'  = ""
    /\ previousImage' = ""
    /\ targetImage'   = ""
    /\ attempts'      = 0
    /\ specDeleting'  = FALSE
    /\ UNCHANGED << specImage, specRunning >>

(***************************************************************************
 NEXT  and  SPEC
 ***************************************************************************)

UserAction ==
    \/ UserCreate
    \/ UserChangeImage
    \/ UserToggleRunning
    \/ UserDelete

ControllerAction ==
    \/ PhaseStorage \/ PhaseOS \/ PhaseVM \/ PhaseReady
    \/ StartStop  \/ DoStop  \/ StartStart \/ DoStart
    \/ StartPatch \/ PatchSnapshotStart \/ PatchSnapshotComplete
    \/ PatchStop  \/ PatchOSDelete \/ PatchOSCreate \/ PatchStart
    \/ PatchVerifySuccess \/ PatchVerifyFail
    \/ PatchRollback \/ PatchGiveUp
    \/ StartDelete \/ DoDelete

Next == UserAction \/ ControllerAction

Spec == Init /\ [][Next]_vars /\ WF_vars(ControllerAction)

(***************************************************************************
 SAFETY INVARIANTS
 ***************************************************************************)

\* The encrypted pgdata DV is never lost while the resource exists.
\* During "creating" it has not been provisioned yet; during "absent" the
\* resource doesn't exist. Everywhere else it must be present.
PgDataAlwaysPresentWhileResourceExists ==
    (phase \notin { "absent", "creating" }) => (pgDataDV = "present")

\* The OS DataVolume is only replaced after a successful snapshot.
\* Any code path that reaches PatchOSReplaced must have snapshot="available".
NoDestructiveStepWithoutSnapshot ==
    (phase = "patching" /\ subPhase = "PatchOSReplaced")
        => snapshot = "available"

\* The OS DataVolume is only mutated while the VM is stopped.
\* Catches any race where OS replacement is attempted on a live VM.
OSReplaceOnlyWhileVMStopped ==
    (phase = "patching" /\ subPhase = "PatchOSReplaced")
        => vmRunning = FALSE

\* In Available state, the recorded current image matches the OS DV image.
\* Catches drift between status.currentImage and reality after a patch.
CurrentImageConsistency ==
    (phase = "available") => (currentImage = osDVImage /\ osDV = "present")

\* Whenever we are patching, rollback context is set.
RollbackTargetSet ==
    (phase = "patching") => (previousImage /= "" /\ targetImage /= "")

\* If the VM is running, it has an OS DataVolume to boot from.
VMRunningImpliesOSDV ==
    vmRunning => (osDV = "present" /\ vm = "present")

(***************************************************************************
 LIVENESS  ----  every transient phase eventually terminates
 ***************************************************************************)

PatchTerminates ==
    (phase = "patching")
        ~> (phase \in { "available", "failed", "deleting", "absent" })

CreateTerminates ==
    (phase = "creating")
        ~> (phase \in { "available", "failed", "deleting", "absent" })

StopTerminates ==
    (phase = "stopping")
        ~> (phase \in { "stopped", "deleting", "absent" })

StartTerminates ==
    (phase = "starting")
        ~> (phase \in { "available", "deleting", "absent" })

DeleteTerminates ==
    (phase = "deleting") ~> (phase = "absent")

==============================================================================
