package state

import (
	"fmt"
	"strings"
	"sync"
	"time"

	eventbus "github.com/Dataman-Cloud/swan/src/event"
	"github.com/Dataman-Cloud/swan/src/mesosproto/mesos"
	"github.com/Dataman-Cloud/swan/src/types"

	"github.com/Sirupsen/logrus"
	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
)

//  TASK_STAGING = 6;  // Initial state. Framework status updates should not use.
//  TASK_STARTING = 0; // The task is being launched by the executor.
//  TASK_RUNNING = 1;
//  TASK_KILLING = 8;  // The task is being killed by the executor.
//  TASK_FINISHED = 2; // TERMINAL: The task finished successfully.
//  TASK_FAILED = 3;   // TERMINAL: The task failed to finish successfully.
//  TASK_KILLED = 4;   // TERMINAL: The task was killed by the executor.
//  TASK_ERROR = 7;    // TERMINAL: The task description contains an error.
//  TASK_LOST = 5;     // TERMINAL: The task failed but can be rescheduled.
//  TASK_DROPPED = 9;  // TERMINAL.
//  TASK_UNREACHABLE = 10;
//  TASK_GONE = 11;    // TERMINAL.
//  TASK_GONE_BY_OPERATOR = 12;
//  TASK_UNKNOWN = 13;

const (
	SLOT_STATE_PENDING_OFFER = "slot_task_pending_offer"
	SLOT_STATE_PENDING_KILL  = "slot_task_pending_killed"
	SLOT_STATE_REAP          = "slot_task_reap"

	SLOT_STATE_TASK_STAGING          = "slot_task_staging"
	SLOT_STATE_TASK_STARTING         = "slot_task_starting"
	SLOT_STATE_TASK_RUNNING          = "slot_task_running"
	SLOT_STATE_TASK_KILLING          = "slot_task_killing"
	SLOT_STATE_TASK_FINISHED         = "slot_task_finished"
	SLOT_STATE_TASK_FAILED           = "slot_task_failed"
	SLOT_STATE_TASK_KILLED           = "slot_task_killed"
	SLOT_STATE_TASK_ERROR            = "slot_task_error"
	SLOT_STATE_TASK_LOST             = "slot_task_lost"
	SLOT_STATE_TASK_DROPPED          = "slot_task_dropped"
	SLOT_STATE_TASK_UNREACHABLE      = "slot_task_unreachable"
	SLOT_STATE_TASK_GONE             = "slot_task_gone"
	SLOT_STATE_TASK_GONE_BY_OPERATOR = "slot_task_gone_by_operator"
	SLOT_STATE_TASK_UNKNOWN          = "slot_task_unknown"
)

type Slot struct {
	Index   int
	ID      string
	App     *App
	Version *types.Version
	State   string

	CurrentTask *Task
	TaskHistory []*Task

	OfferID       string
	AgentID       string
	Ip            string
	AgentHostName string

	resourceReservationLock sync.Mutex

	restartPolicy *RestartPolicy

	healthy bool

	inTransaction bool
	touched       bool
}

type SlotsById []*Slot

func (a SlotsById) Len() int           { return len(a) }
func (a SlotsById) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a SlotsById) Less(i, j int) bool { return a[i].Index < a[j].Index }

type SlotResource struct {
	CPU  float64
	Mem  float64
	Disk float64
}

func NewSlot(app *App, version *types.Version, index int) *Slot {
	slot := &Slot{
		Index:       index,
		App:         app,
		Version:     version,
		TaskHistory: make([]*Task, 0),
		ID:          fmt.Sprintf("%d-%s", index, app.ID),

		resourceReservationLock: sync.Mutex{},

		inTransaction: false,
		touched:       true,
	}

	if slot.App.IsFixed() {
		slot.Ip = app.CurrentVersion.IP[index]
	}

	// initialize restart policy
	testAndRestartFunc := func(s *Slot) bool {
		if slot.Abnormal() {
			s.Archive()
			s.DispatchNewTask(slot.Version)
		}

		return false
	}

	//slot.restartPolicy = NewRestartPolicy(slot, slot.Version.BackoffSeconds,
	//slot.Version.BackoffFactor, slot.Version.MaxLaunchDelaySeconds, testAndRestartFunc)
	slot.restartPolicy = NewRestartPolicy(slot, time.Second*10, 1, time.Second*300, testAndRestartFunc)

	slot.create()

	return slot
}

// kill task doesn't need cleanup slot from app.Slots
func (slot *Slot) KillTask() {
	slot.BeginTx()
	defer slot.Commit()

	slot.StopRestartPolicy()

	if slot.Dispatched() {
		slot.SetState(SLOT_STATE_PENDING_KILL)
		slot.CurrentTask.Kill()
	} else {
		slot.SetState(SLOT_STATE_REAP)
	}
}

func (slot *Slot) Archive() {
	slot.BeginTx()
	defer slot.Commit()

	slot.CurrentTask.ArchivedAt = time.Now()
	slot.TaskHistory = append(slot.TaskHistory, slot.CurrentTask)
	WithConvertTask(context.TODO(), slot.CurrentTask, nil, persistentStore.UpdateTask)
}

func (slot *Slot) DispatchNewTask(version *types.Version) {
	slot.BeginTx()
	defer slot.Commit()

	slot.Version = version
	slot.CurrentTask = NewTask(slot.Version, slot)
	slot.SetState(SLOT_STATE_PENDING_OFFER)

	OfferAllocatorInstance().PutSlotBackToPendingQueue(slot)
}

func (slot *Slot) TestOfferMatch(ow *OfferWrapper) bool {
	constraintsMatch := true
	if len(slot.Version.Constraints) > 0 {
		evalStatement, err := ParseConstraint(strings.ToLower(slot.Version.Constraints))
		if err != nil {
			logrus.Errorf("fail to found offer due to malformat constraints")
			return false
		}

		evalStatement.SetContext(&ConstraintParamHolder{
			Slot:  slot,
			Offer: ow.Offer,
		})

		constraintsMatch = evalStatement.Eval()
	}

	resourcesMatch := ow.CpuRemain() >= slot.Version.CPUs &&
		ow.MemRemain() >= slot.Version.Mem &&
		ow.DiskRemain() >= slot.Version.Disk

	portsMatch := true
	if slot.App.IsReplicates() && len(ow.PortsRemain()) < len(slot.Version.Container.Docker.PortMappings) {
		portsMatch = false
	}

	return constraintsMatch && resourcesMatch && portsMatch
}

func (slot *Slot) ReserveOfferAndPrepareTaskInfo(ow *OfferWrapper) (*OfferWrapper, *mesos.TaskInfo) {
	slot.resourceReservationLock.Lock()
	defer slot.resourceReservationLock.Unlock()

	ow.CpusUsed += slot.Version.CPUs
	ow.MemUsed += slot.Version.Mem
	ow.DiskUsed += slot.Version.Disk

	taskInfo := slot.CurrentTask.PrepareTaskInfo(ow)

	if err := slot.UpdateOfferInfo(ow.Offer); err != nil {
		logrus.Errorf("update offer info of slot: %d failed, Error: %s", slot.Index, err.Error())
	}

	if slot.App.IsReplicates() { // reserve port only for replicates application
		ow.PortUsedSize += len(slot.Version.Container.Docker.PortMappings)
	}

	return ow, taskInfo
}

func (slot *Slot) UpdateOfferInfo(offer *mesos.Offer) error {
	slot.OfferID = *offer.GetId().Value
	slot.CurrentTask.OfferID = *offer.GetId().Value

	slot.AgentID = *offer.GetAgentId().Value
	slot.CurrentTask.AgentID = *offer.GetAgentId().Value

	slot.AgentHostName = offer.GetHostname()
	slot.CurrentTask.AgentHostName = offer.GetHostname()

	return WithConvertSlot(context.TODO(), slot, nil, persistentStore.UpdateSlot)
}

func (slot *Slot) ResourcesNeeded() []*mesos.Resource {
	var resources = []*mesos.Resource{}

	if slot.Version.CPUs > 0 {
		resources = append(resources, buildScalarResource("cpus", slot.Version.CPUs))
	}

	if slot.Version.Mem > 0 {
		resources = append(resources, buildScalarResource("mem", slot.Version.Mem))
	}

	if slot.Version.Disk > 0 {
		resources = append(resources, buildScalarResource("disk", slot.Version.Disk))
	}

	return resources
}

func (slot *Slot) ResourcesUsed() *SlotResource {
	var slotResource SlotResource
	if slot.StateIs(SLOT_STATE_TASK_STAGING) ||
		slot.StateIs(SLOT_STATE_TASK_STARTING) ||
		slot.StateIs(SLOT_STATE_TASK_RUNNING) ||
		slot.StateIs(SLOT_STATE_TASK_KILLING) {
		slotResource.CPU = slot.Version.CPUs
		slotResource.Mem = slot.Version.Mem
		slotResource.Disk = slot.Version.Disk

		// TODO(xychu): add usage stats
	}
	return &slotResource
}

func (slot *Slot) StateIs(state string) bool {
	return slot.State == state
}

func (slot *Slot) SetState(state string) error {
	logrus.Debug("setting state for slot %s from %s to %s", slot.ID, slot.State, state)

	slot.State = state
	switch slot.State {
	case SLOT_STATE_PENDING_OFFER:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStatePendingOffer)
	case SLOT_STATE_PENDING_KILL:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStatePendingKill)
	case SLOT_STATE_REAP:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateReap)
	case SLOT_STATE_TASK_STAGING:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateStaging)
	case SLOT_STATE_TASK_STARTING:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateStarting)
	case SLOT_STATE_TASK_RUNNING:
		if slot.Version.HealthCheck == nil {
			slot.SetHealthy(true)
		}
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateRunning)
	case SLOT_STATE_TASK_KILLING:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateKilling)
	case SLOT_STATE_TASK_FINISHED:
		slot.StopRestartPolicy()
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateFinished)
	case SLOT_STATE_TASK_FAILED:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateFailed)
	case SLOT_STATE_TASK_KILLED:
		slot.StopRestartPolicy()
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateKilled)
	case SLOT_STATE_TASK_ERROR:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateError)
	case SLOT_STATE_TASK_LOST:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateLost)
	case SLOT_STATE_TASK_DROPPED:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateDropped)
	case SLOT_STATE_TASK_UNREACHABLE:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateUnreachable)
	case SLOT_STATE_TASK_GONE:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateGone)
	case SLOT_STATE_TASK_GONE_BY_OPERATOR:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateGoneByOperator)
	case SLOT_STATE_TASK_UNKNOWN:
		slot.EmitTaskEvent(eventbus.EventTypeTaskStateUnknown)
	default:
	}

	// skip app invalidation if slot state is not mesos driven
	if (slot.State != SLOT_STATE_PENDING_OFFER) ||
		(slot.State != SLOT_STATE_PENDING_KILL) {
		slot.App.Step()
	}

	slot.Touch(false)
	return nil
}

func (slot *Slot) StopRestartPolicy() {
	if slot.restartPolicy != nil {
		slot.restartPolicy.Stop()
		slot.restartPolicy = nil
	}
}

func (slot *Slot) Abnormal() bool {
	return slot.StateIs(SLOT_STATE_TASK_LOST) ||
		slot.StateIs(SLOT_STATE_TASK_FAILED) ||
		slot.StateIs(SLOT_STATE_TASK_LOST) ||
		slot.StateIs(SLOT_STATE_TASK_FINISHED) ||
		slot.StateIs(SLOT_STATE_TASK_KILLED) ||
		slot.StateIs(SLOT_STATE_TASK_DROPPED) ||
		slot.StateIs(SLOT_STATE_TASK_UNKNOWN) ||
		slot.StateIs(SLOT_STATE_TASK_UNREACHABLE) ||
		slot.StateIs(SLOT_STATE_TASK_GONE_BY_OPERATOR) ||
		slot.StateIs(SLOT_STATE_TASK_GONE) ||
		slot.StateIs(SLOT_STATE_TASK_FINISHED) ||
		slot.StateIs(SLOT_STATE_REAP)
}

func (slot *Slot) Dispatched() bool {
	return slot.StateIs(SLOT_STATE_TASK_RUNNING) ||
		slot.StateIs(SLOT_STATE_TASK_STARTING) ||
		slot.StateIs(SLOT_STATE_TASK_STAGING) ||
		slot.StateIs(SLOT_STATE_TASK_FAILED) ||
		slot.StateIs(SLOT_STATE_TASK_LOST) ||
		slot.StateIs(SLOT_STATE_TASK_FINISHED) ||
		slot.StateIs(SLOT_STATE_TASK_KILLED) ||
		slot.StateIs(SLOT_STATE_TASK_DROPPED) ||
		slot.StateIs(SLOT_STATE_TASK_UNKNOWN) ||
		slot.StateIs(SLOT_STATE_TASK_UNREACHABLE) ||
		slot.StateIs(SLOT_STATE_TASK_GONE_BY_OPERATOR) ||
		slot.StateIs(SLOT_STATE_TASK_GONE) ||
		slot.StateIs(SLOT_STATE_TASK_FINISHED)
}

func (slot *Slot) EmitTaskEvent(eventType string) {
	eventbus.WriteEvent(slot.BuildTaskEvent(eventType))
}

func (slot *Slot) BuildTaskEvent(eventType string) *eventbus.Event {
	e := &eventbus.Event{
		Type:    eventType,
		AppID:   slot.App.ID,
		AppMode: string(slot.App.Mode),
	}

	payload := &types.TaskInfoEvent{
		TaskID:    slot.ID,
		AppID:     slot.App.ID,
		State:     slot.State,
		Healthy:   slot.healthy,
		ClusterID: slot.App.ClusterID,
		RunAs:     slot.Version.RunAs,
	}

	if slot.App.IsFixed() {
		payload.IP = slot.Ip
		payload.Mode = string(APP_MODE_FIXED)
	} else {
		payload.IP = slot.AgentHostName
		payload.Mode = string(APP_MODE_REPLICATES)
		if len(slot.CurrentTask.HostPorts) > 0 {
			payload.Port = uint32(slot.CurrentTask.HostPorts[0])
			payload.PortName = slot.Version.Container.Docker.PortMappings[0].Name
		}
	}

	e.Payload = payload

	return e
}

func (slot *Slot) Healthy() bool {
	return slot.healthy
}

func (slot *Slot) SetHealthy(healthy bool) {
	slot.healthy = healthy
	if healthy {
		slot.EmitTaskEvent(eventbus.EventTypeTaskHealthy)
	} else {
		slot.EmitTaskEvent(eventbus.EventTypeTaskUnhealthy)
	}
	slot.App.Step() // step forward state-machine
	slot.Touch(false)
}

func (slot *Slot) Remove() {
	slot.remove()
}

func (slot *Slot) Touch(force bool) {
	if force { // force update the app
		slot.update()
		return
	}

	if slot.inTransaction {
		slot.touched = true
		logrus.Infof("delay update action as current slot in between tranaction")
	} else {
		slot.update()
	}
}

func (slot *Slot) BeginTx() {
	slot.inTransaction = true
}

func (slot *Slot) ServiceDiscoveryURL() string {
	return strings.ToLower(strings.Replace(slot.ID, "-", ".", -1))
}

// here we persist the app anyway, no matter it touched or not
func (slot *Slot) Commit() {
	slot.inTransaction = false
	slot.touched = false
	slot.update()
}

func (slot *Slot) update() {
	logrus.Debugf("update slot %s", slot.ID)
	WithConvertSlot(context.TODO(), slot, nil, persistentStore.UpdateSlot)
	slot.touched = false
}

func (slot *Slot) create() {
	logrus.Debugf("create slot %s", slot.ID)
	WithConvertSlot(context.TODO(), slot, nil, persistentStore.CreateSlot)
	slot.touched = false
}

func (slot *Slot) remove() {
	logrus.Debugf("remove slot %s", slot.ID)
	persistentStore.DeleteSlot(context.TODO(), slot.App.ID, slot.ID, nil)
	slot.touched = false
}

func buildScalarResource(name string, value float64) *mesos.Resource {
	return &mesos.Resource{
		Name:   &name,
		Type:   mesos.Value_SCALAR.Enum(),
		Scalar: &mesos.Value_Scalar{Value: &value},
	}
}

func buildRangeResource(name string, begin, end uint64) *mesos.Resource {
	return &mesos.Resource{
		Name: &name,
		Type: mesos.Value_RANGES.Enum(),
		Ranges: &mesos.Value_Ranges{
			Range: []*mesos.Value_Range{
				{
					Begin: proto.Uint64(begin),
					End:   proto.Uint64(end),
				},
			},
		},
	}
}
