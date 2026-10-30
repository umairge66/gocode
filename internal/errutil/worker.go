package errutil

import "fmt"

// -----------------------------------------------------------------------------
// Worker / supervisor result codes
//
// The supervisor replies with result objects {ok:false, error:<msg>, code:<E…>}.
// The codes form a closed set; they are modelled as a WorkerCode type and
// carried by WorkerError.
// -----------------------------------------------------------------------------

// WorkerCode is a supervisor/worker IPC failure code.
type WorkerCode string

const (
	WorkerEAuth       WorkerCode = "EAUTH"       // authentication failed
	WorkerECwdGone    WorkerCode = "ECWDGONE"    // working directory no longer exists
	WorkerENoJob      WorkerCode = "ENOJOB"      // job not found (may have exited)
	WorkerENoReply    WorkerCode = "ENOREPLY"    // worker produced no reply
	WorkerEPeerUID    WorkerCode = "EPEERUID"    // peer uid mismatch
	WorkerEProto      WorkerCode = "EPROTO"      // protocol error
	WorkerERespawning WorkerCode = "ERESPAWNING" // worker restarting; retry
	WorkerEStale      WorkerCode = "ESTALE"      // stale handle/record
	WorkerEStarting   WorkerCode = "ESTARTING"   // worker still starting
	WorkerETimeout    WorkerCode = "ETIMEOUT"    // operation timed out
	WorkerETooLarge   WorkerCode = "ETOOLARGE"   // payload too large
	WorkerEUnknown    WorkerCode = "EUNKNOWN"    // unclassified failure
)

// WorkerError carries a supervisor reply failure (the {ok:false,error,code}
// shape). Code is one of the WorkerCode constants.
type WorkerError struct {
	Code    WorkerCode
	Message string
	Cause   error
}

func (e *WorkerError) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

func (e *WorkerError) Unwrap() error { return e.Cause }

// Is lets errors.Is(err, &WorkerError{Code: WorkerERespawning}) match on code.
func (e *WorkerError) Is(target error) bool {
	t, ok := target.(*WorkerError)
	return ok && t.Code != "" && t.Code == e.Code
}

// NewWorkerError builds a WorkerError.
func NewWorkerError(code WorkerCode, message string) *WorkerError {
	return &WorkerError{Code: code, Message: message}
}

// -----------------------------------------------------------------------------
// Filesystem / OS errno handling

// CodeNotOwned is the errno-style code attached when a path that must be owned
// by the current uid is owned by another user.
const CodeNotOwned = "ENOTOWNED"

// NotOwnedError reports that a path required to be owned by the current uid was
// owned by a different uid.
type NotOwnedError struct {
	Path string
	UID  int
}

func (e *NotOwnedError) Error() string {
	return fmt.Sprintf("refusing to bind: %s is owned by uid %d", e.Path, e.UID)
}

// Code reports the error's `.code` value.
func (e *NotOwnedError) Code() string { return CodeNotOwned }
