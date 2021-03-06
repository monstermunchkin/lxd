package drivers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/flosch/pongo2"
	"github.com/gorilla/websocket"
	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v2"

	lxdClient "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/db/query"
	"github.com/lxc/lxd/lxd/device"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/drivers/qmp"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/instance/operationlock"
	"github.com/lxc/lxd/lxd/maas"
	"github.com/lxc/lxd/lxd/network"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/resources"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	storagePools "github.com/lxc/lxd/lxd/storage"
	storageDrivers "github.com/lxc/lxd/lxd/storage/drivers"
	pongoTemplate "github.com/lxc/lxd/lxd/template"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/lxd/vsock"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/containerwriter"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/osarch"
	"github.com/lxc/lxd/shared/termios"
	"github.com/lxc/lxd/shared/units"
)

// qemuAsyncIO is used to indicate disk should use unsafe cache I/O.
const qemuUnsafeIO = "unsafeio"

var errQemuAgentOffline = fmt.Errorf("LXD VM agent isn't currently running")

var vmConsole = map[int]bool{}
var vmConsoleLock sync.Mutex

// qemuLoad creates a Qemu instance from the supplied InstanceArgs.
func qemuLoad(s *state.State, args db.InstanceArgs, profiles []api.Profile) (instance.Instance, error) {
	// Create the instance struct.
	vm := qemuInstantiate(s, args, nil)

	// Expand config and devices.
	err := vm.expandConfig(profiles)
	if err != nil {
		return nil, err
	}

	err = vm.expandDevices(profiles)
	if err != nil {
		return nil, err
	}

	return vm, nil
}

// qemuInstantiate creates a Qemu struct without expanding config. The expandedDevices argument is
// used during device config validation when the devices have already been expanded and we do not
// have access to the profiles used to do it. This can be safely passed as nil if not required.
func qemuInstantiate(s *state.State, args db.InstanceArgs, expandedDevices deviceConfig.Devices) *qemu {
	vm := &qemu{
		common: common{
			dbType:       args.Type,
			localConfig:  args.Config,
			localDevices: args.Devices,
			project:      args.Project,
			state:        s,
			profiles:     args.Profiles,
		},
		id:           args.ID,
		name:         args.Name,
		description:  args.Description,
		ephemeral:    args.Ephemeral,
		architecture: args.Architecture,
		snapshot:     args.Snapshot,
		creationDate: args.CreationDate,
		lastUsedDate: args.LastUsedDate,
		stateful:     args.Stateful,
		node:         args.Node,
		expiryDate:   args.ExpiryDate,
	}

	// Get the architecture name.
	archName, err := osarch.ArchitectureName(vm.architecture)
	if err == nil {
		vm.architectureName = archName
	}

	// Cleanup the zero values.
	if vm.expiryDate.IsZero() {
		vm.expiryDate = time.Time{}
	}

	if vm.creationDate.IsZero() {
		vm.creationDate = time.Time{}
	}

	if vm.lastUsedDate.IsZero() {
		vm.lastUsedDate = time.Time{}
	}

	// This is passed during expanded config validation.
	if expandedDevices != nil {
		vm.expandedDevices = expandedDevices
	}

	return vm
}

// qemuCreate creates a new storage volume record and returns an initialised Instance.
func qemuCreate(s *state.State, args db.InstanceArgs) (instance.Instance, error) {
	// Create the instance struct.
	vm := &qemu{
		common: common{
			dbType:       args.Type,
			localConfig:  args.Config,
			localDevices: args.Devices,
			state:        s,
			profiles:     args.Profiles,
			project:      args.Project,
		},
		id:           args.ID,
		name:         args.Name,
		node:         args.Node,
		description:  args.Description,
		ephemeral:    args.Ephemeral,
		architecture: args.Architecture,
		snapshot:     args.Snapshot,
		stateful:     args.Stateful,
		creationDate: args.CreationDate,
		lastUsedDate: args.LastUsedDate,
		expiryDate:   args.ExpiryDate,
	}

	// Get the architecture name.
	archName, err := osarch.ArchitectureName(vm.architecture)
	if err == nil {
		vm.architectureName = archName
	}

	// Cleanup the zero values.
	if vm.expiryDate.IsZero() {
		vm.expiryDate = time.Time{}
	}

	if vm.creationDate.IsZero() {
		vm.creationDate = time.Time{}
	}

	if vm.lastUsedDate.IsZero() {
		vm.lastUsedDate = time.Time{}
	}

	ctxMap := log.Ctx{
		"project":   args.Project,
		"name":      vm.name,
		"ephemeral": vm.ephemeral,
	}

	logger.Info("Creating instance", ctxMap)

	revert := true
	defer func() {
		if !revert {
			return
		}

		vm.Delete()
	}()

	// Load the config.
	err = vm.init()
	if err != nil {
		logger.Error("Failed creating instance", ctxMap)
		return nil, err
	}

	// Validate expanded config.
	err = instance.ValidConfig(s.OS, vm.expandedConfig, false, true)
	if err != nil {
		logger.Error("Failed creating instance", ctxMap)
		return nil, err
	}

	err = instance.ValidDevices(s, s.Cluster, vm.Type(), vm.expandedDevices, true)
	if err != nil {
		logger.Error("Failed creating instance", ctxMap)
		return nil, errors.Wrap(err, "Invalid devices")
	}

	// Retrieve the instance's storage pool.
	_, rootDiskDevice, err := shared.GetRootDiskDevice(vm.expandedDevices.CloneNative())
	if err != nil {
		return nil, err
	}

	if rootDiskDevice["pool"] == "" {
		return nil, fmt.Errorf("The instances's root device is missing the pool property")
	}

	storagePool := rootDiskDevice["pool"]

	// Get the storage pool ID for the instance.
	poolID, pool, err := s.Cluster.StoragePoolGet(storagePool)
	if err != nil {
		return nil, err
	}

	// Fill in any default volume config.
	volumeConfig := map[string]string{}
	err = storagePools.VolumeFillDefault(storagePool, volumeConfig, pool)
	if err != nil {
		return nil, err
	}

	// Create a new database entry for the instance's storage volume.
	_, err = s.Cluster.StoragePoolVolumeCreate(args.Project, args.Name, "", db.StoragePoolVolumeTypeVM, vm.IsSnapshot(), poolID, volumeConfig)
	if err != nil {
		return nil, err
	}

	if !vm.IsSnapshot() {
		// Update MAAS.
		err = vm.maasUpdate(nil)
		if err != nil {
			logger.Error("Failed creating instance", ctxMap)
			return nil, err
		}

		// Add devices to instance.
		for k, m := range vm.expandedDevices {
			err = vm.deviceAdd(k, m)
			if err != nil && err != device.ErrUnsupportedDevType {
				return nil, errors.Wrapf(err, "Failed to add device '%s'", k)
			}
		}
	}

	logger.Info("Created instance", ctxMap)
	vm.state.Events.SendLifecycle(vm.project, "virtual-machine-created",
		fmt.Sprintf("/1.0/virtual-machines/%s", vm.name), nil)

	revert = false
	return vm, nil
}

// qemu is the QEMU virtual machine driver.
type qemu struct {
	common

	// Properties.
	architecture int
	snapshot     bool
	creationDate time.Time
	lastUsedDate time.Time
	ephemeral    bool
	id           int
	name         string
	description  string
	stateful     bool

	// Clustering.
	node string

	// Progress tracking.
	op *operations.Operation

	expiryDate time.Time

	// Cached handles.
	// Do not use these variables directly, instead use their associated get functions so they
	// will be initialised on demand.
	agentClient      *http.Client
	storagePool      storagePools.Pool
	architectureName string
}

// getAgentClient returns the current agent client handle. To avoid TLS setup each time this
// function is called, the handle is cached internally in the Qemu struct.
func (vm *qemu) getAgentClient() (*http.Client, error) {
	if vm.agentClient != nil {
		return vm.agentClient, nil
	}

	// The connection uses mutual authentication, so use the LXD server's key & cert for client.
	agentCert, _, clientCert, clientKey, err := vm.generateAgentCert()
	if err != nil {
		return nil, err
	}

	agent, err := vsock.HTTPClient(vm.vsockID(), clientCert, clientKey, agentCert)
	if err != nil {
		return nil, err
	}

	return agent, nil
}

// getStoragePool returns the current storage pool handle. To avoid a DB lookup each time this
// function is called, the handle is cached internally in the Qemu struct.
func (vm *qemu) getStoragePool() (storagePools.Pool, error) {
	if vm.storagePool != nil {
		return vm.storagePool, nil
	}

	pool, err := storagePools.GetPoolByInstance(vm.state, vm)
	if err != nil {
		return nil, err
	}
	vm.storagePool = pool

	return vm.storagePool, nil
}

func (vm *qemu) getMonitorEventHandler() func(event string, data map[string]interface{}) {
	id := vm.id
	state := vm.state

	return func(event string, data map[string]interface{}) {
		if !shared.StringInSlice(event, []string{"SHUTDOWN"}) {
			return
		}

		inst, err := instance.LoadByID(state, id)
		if err != nil {
			logger.Errorf("Failed to load instance with id=%d", id)
			return
		}

		if event == "SHUTDOWN" {
			target := "stop"
			entry, ok := data["reason"]
			if ok && entry == "guest-reset" {
				target = "reboot"
			}

			err = inst.(*qemu).OnStop(target)
			if err != nil {
				logger.Errorf("Failed to cleanly stop instance '%s': %v", project.Instance(inst.Project(), inst.Name()), err)
				return
			}
		}
	}
}

// mount the instance's config volume if needed.
func (vm *qemu) mount() (bool, error) {
	var pool storagePools.Pool
	pool, err := vm.getStoragePool()
	if err != nil {
		return false, err
	}

	ourMount, err := pool.MountInstance(vm, nil)
	if err != nil {
		return false, err
	}

	return ourMount, nil
}

// unmount the instance's config volume if needed.
func (vm *qemu) unmount() (bool, error) {
	pool, err := vm.getStoragePool()
	if err != nil {
		return false, err
	}

	unmounted, err := pool.UnmountInstance(vm, nil)
	if err != nil {
		return false, err
	}

	return unmounted, nil
}

// generateAgentCert creates the necessary server key and certificate if needed.
func (vm *qemu) generateAgentCert() (string, string, string, string, error) {
	// Mount the instance's config volume if needed.
	ourMount, err := vm.mount()
	if err != nil {
		return "", "", "", "", err
	}

	if ourMount {
		defer vm.unmount()
	}

	agentCertFile := filepath.Join(vm.Path(), "agent.crt")
	agentKeyFile := filepath.Join(vm.Path(), "agent.key")
	clientCertFile := filepath.Join(vm.Path(), "agent-client.crt")
	clientKeyFile := filepath.Join(vm.Path(), "agent-client.key")

	// Create server certificate.
	err = shared.FindOrGenCert(agentCertFile, agentKeyFile, false, false)
	if err != nil {
		return "", "", "", "", err
	}

	// Create client certificate.
	err = shared.FindOrGenCert(clientCertFile, clientKeyFile, true, false)
	if err != nil {
		return "", "", "", "", err
	}

	// Read all the files
	agentCert, err := ioutil.ReadFile(agentCertFile)
	if err != nil {
		return "", "", "", "", err
	}

	agentKey, err := ioutil.ReadFile(agentKeyFile)
	if err != nil {
		return "", "", "", "", err
	}

	clientCert, err := ioutil.ReadFile(clientCertFile)
	if err != nil {
		return "", "", "", "", err
	}

	clientKey, err := ioutil.ReadFile(clientKeyFile)
	if err != nil {
		return "", "", "", "", err
	}

	return string(agentCert), string(agentKey), string(clientCert), string(clientKey), nil
}

// Freeze freezes the instance.
func (vm *qemu) Freeze() error {
	// Connect to the monitor.
	monitor, err := qmp.Connect(vm.getMonitorPath(), vm.getMonitorEventHandler())
	if err != nil {
		return err
	}

	// Send the stop command.
	err = monitor.Pause()
	if err != nil {
		return err
	}

	return nil
}

// OnStop is run when the instance stops.
func (vm *qemu) OnStop(target string) error {
	// Pick up the existing stop operation lock created in Stop() function.
	op := operationlock.Get(vm.id)
	if op != nil && op.Action() != "stop" {
		return fmt.Errorf("Instance is already running a %s operation", op.Action())
	}

	// Cleanup.
	vm.cleanupDevices()
	os.Remove(vm.pidFilePath())
	os.Remove(vm.getMonitorPath())
	vm.unmount()

	// Record power state.
	err := vm.state.Cluster.ContainerSetState(vm.id, "STOPPED")
	if err != nil {
		op.Done(err)
		return err
	}

	if target == "reboot" {
		err := vm.Start(false)
		if err != nil {
			return err
		}
	}

	if op != nil {
		op.Done(nil)
	}

	return nil
}

// Shutdown shuts the instance down.
func (vm *qemu) Shutdown(timeout time.Duration) error {
	if !vm.IsRunning() {
		return fmt.Errorf("The instance is already stopped")
	}

	// Setup a new operation
	op, err := operationlock.Create(vm.id, "stop", true, true)
	if err != nil {
		return err
	}

	// Connect to the monitor.
	monitor, err := qmp.Connect(vm.getMonitorPath(), vm.getMonitorEventHandler())
	if err != nil {
		op.Done(err)
		return err
	}

	// Get the wait channel.
	chDisconnect, err := monitor.Wait()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			op.Done(nil)
			return nil
		}

		op.Done(err)
		return err
	}

	// Send the system_powerdown command.
	err = monitor.Powerdown()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			op.Done(nil)
			return nil
		}

		op.Done(err)
		return err
	}

	// If timeout provided, block until the VM is not running or the timeout has elapsed.
	if timeout > 0 {
		select {
		case <-chDisconnect:
			op.Done(nil)
			vm.state.Events.SendLifecycle(vm.project, "instance-shutdown", fmt.Sprintf("/1.0/virtual-machines/%s", vm.name), nil)
			return nil
		case <-time.After(timeout):
			op.Done(fmt.Errorf("Instance was not shutdown after timeout"))
			return fmt.Errorf("Instance was not shutdown after timeout")
		}
	} else {
		<-chDisconnect // Block until VM is not running if no timeout provided.
	}

	op.Done(nil)
	vm.state.Events.SendLifecycle(vm.project, "instance-shutdown", fmt.Sprintf("/1.0/virtual-machines/%s", vm.name), nil)
	return nil
}

func (vm *qemu) ovmfPath() string {
	if os.Getenv("LXD_OVMF_PATH") != "" {
		return os.Getenv("LXD_OVMF_PATH")
	}

	return "/usr/share/OVMF"
}

// Start starts the instance.
func (vm *qemu) Start(stateful bool) error {
	// Ensure the correct vhost_vsock kernel module is loaded before establishing the vsock.
	err := util.LoadModule("vhost_vsock")
	if err != nil {
		return err
	}

	if vm.IsRunning() {
		return fmt.Errorf("The instance is already running")
	}

	// Setup a new operation
	op, err := operationlock.Create(vm.id, "start", false, false)
	if err != nil {
		return errors.Wrap(err, "Create instance start operation")
	}
	defer op.Done(nil)

	revert := revert.New()
	defer revert.Fail()

	// Mount the instance's config volume.
	_, err = vm.mount()
	if err != nil {
		op.Done(err)
		return err
	}

	revert.Add(func() { vm.unmount() })

	err = vm.generateConfigShare()
	if err != nil {
		op.Done(err)
		return err
	}

	err = os.MkdirAll(vm.LogPath(), 0700)
	if err != nil {
		op.Done(err)
		return err
	}

	err = os.MkdirAll(vm.DevicesPath(), 0711)
	if err != nil {
		op.Done(err)
		return err
	}

	err = os.MkdirAll(vm.ShmountsPath(), 0711)
	if err != nil {
		op.Done(err)
		return err
	}

	// Get a UUID for Qemu.
	vmUUID := vm.localConfig["volatile.vm.uuid"]
	if vmUUID == "" {
		vmUUID = uuid.New()
		vm.VolatileSet(map[string]string{"volatile.vm.uuid": vmUUID})
	}

	// Copy OVMF settings firmware to nvram file.
	// This firmware file can be modified by the VM so it must be copied from the defaults.
	if !shared.PathExists(vm.getNvramPath()) {
		err = vm.setupNvram()
		if err != nil {
			op.Done(err)
			return err
		}
	}

	devConfs := make([]*deviceConfig.RunConfig, 0, len(vm.expandedDevices))

	// Setup devices in sorted order, this ensures that device mounts are added in path order.
	for _, dev := range vm.expandedDevices.Sorted() {
		// Start the device.
		runConf, err := vm.deviceStart(dev.Name, dev.Config, false)
		if err != nil {
			op.Done(err)
			return errors.Wrapf(err, "Failed to start device %q", dev.Name)
		}

		if runConf == nil {
			continue
		}

		// Use a local function argument to ensure the current device is added to the reverter.
		func(localDev deviceConfig.DeviceNamed) {
			revert.Add(func() {
				err := vm.deviceStop(localDev.Name, localDev.Config)
				if err != nil {
					logger.Errorf("Failed to cleanup device %q: %v", localDev.Name, err)
				}
			})
		}(dev)

		devConfs = append(devConfs, runConf)
	}

	// Get qemu configuration.
	qemuBinary, err := vm.qemuArchConfig()
	if err != nil {
		op.Done(err)
		return err
	}

	// Define a set of files to open and pass their file descriptors to qemu command.
	fdFiles := make([]string, 0)

	confFile, err := vm.generateQemuConfigFile(devConfs, &fdFiles)
	if err != nil {
		op.Done(err)
		return err
	}

	// Check qemu is installed.
	qemuPath, err := exec.LookPath(qemuBinary)
	if err != nil {
		op.Done(err)
		return err
	}

	qemuCmd := []string{
		"--",
		qemuPath,
		"-S",
		"-name", vm.Name(),
		"-uuid", vmUUID,
		"-daemonize",
		"-cpu", "host",
		"-nographic",
		"-serial", "chardev:console",
		"-nodefaults",
		"-no-reboot",
		"-no-user-config",
		"-sandbox", "on,obsolete=deny,elevateprivileges=allow,spawn=deny,resourcecontrol=deny",
		"-readconfig", confFile,
		"-pidfile", vm.pidFilePath(),
		"-D", vm.LogFilePath(),
		"-chroot", vm.Path(),
	}

	// Attempt to drop privileges.
	if vm.state.OS.UnprivUser != "" {
		qemuCmd = append(qemuCmd, "-runas", vm.state.OS.UnprivUser)

		// Change ownership of config directory files so they are accessible to the
		// unprivileged qemu process so that the 9p share can work.
		//
		// Security note: The 9P share will present the UID owner of these files on the host
		// to the VM. In order to ensure that non-root users in the VM cannot access these
		// files be sure to mount the 9P share in the VM with the "access=0" option to allow
		// only root user in VM to access the mounted share.
		err := filepath.Walk(filepath.Join(vm.Path(), "config"),
			func(path string, info os.FileInfo, err error) error {
				if err != nil {
					op.Done(err)
					return err
				}

				err = os.Chown(path, vm.state.OS.UnprivUID, -1)
				if err != nil {
					op.Done(err)
					return err
				}

				return nil
			})
		if err != nil {
			op.Done(err)
			return err
		}
	}

	if shared.IsTrue(vm.expandedConfig["limits.memory.hugepages"]) {
		qemuCmd = append(qemuCmd, "-mem-path", "/dev/hugepages/", "-mem-prealloc")
	}

	if vm.expandedConfig["raw.qemu"] != "" {
		fields := strings.Split(vm.expandedConfig["raw.qemu"], " ")
		qemuCmd = append(qemuCmd, fields...)
	}

	// Run the qemu command via forklimits so we can selectively increase ulimits.
	forkLimitsCmd := []string{
		"forklimits",
		"limit=memlock:unlimited:unlimited", // Required for PCI passthrough.
	}

	for i := range fdFiles {
		// Pass through any file descriptors as 3+i (as first 3 file descriptors are taken as standard).
		forkLimitsCmd = append(forkLimitsCmd, fmt.Sprintf("fd=%d", 3+i))
	}

	cmd := exec.Command(vm.state.OS.ExecPath, append(forkLimitsCmd, qemuCmd...)...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Open any extra files and pass their file handles to qemu command.
	for _, file := range fdFiles {
		info, err := os.Stat(file)
		if err != nil {
			err = errors.Wrapf(err, "Error detecting file type %q", file)
			op.Done(err)
			return err
		}

		var f *os.File
		mode := info.Mode()
		if mode&os.ModeSocket != 0 {
			c, err := vm.openUnixSocket(file)
			if err != nil {
				err = errors.Wrapf(err, "Error opening socket file %q", file)
				op.Done(err)
				return err
			}

			f, err = c.File()
			if err != nil {
				err = errors.Wrapf(err, "Error getting socket file descriptor %q", file)
				op.Done(err)
				return err
			}
			defer c.Close()
			defer f.Close() // Close file after qemu has started.
		} else {
			f, err = os.OpenFile(file, os.O_RDWR, 0)
			if err != nil {
				err = errors.Wrapf(err, "Error opening exta file %q", file)
				op.Done(err)
				return err
			}
			defer f.Close() // Close file after qemu has started.
		}

		cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	}

	err = cmd.Run()
	if err != nil {
		err = errors.Wrapf(err, "Failed to run: %s: %s", strings.Join(cmd.Args, " "), strings.TrimSpace(string(stderr.Bytes())))
		op.Done(err)
		return err
	}

	pid, err := vm.pid()
	if err != nil {
		logger.Errorf(`Failed to get VM process ID "%d"`, pid)
		return err
	}

	revert.Add(func() {
		proc, err := os.FindProcess(pid)
		if err != nil {
			logger.Errorf(`Failed to find VM process "%d"`, pid)
			return
		}

		proc.Kill()
		if err != nil {
			logger.Errorf(`Failed to kill VM process "%d"`, pid)
		}
	})

	// Start QMP monitoring.
	monitor, err := qmp.Connect(vm.getMonitorPath(), vm.getMonitorEventHandler())
	if err != nil {
		op.Done(err)
		return err
	}

	// Apply CPU pinning.
	cpuLimit, ok := vm.expandedConfig["limits.cpu"]
	if ok && cpuLimit != "" {
		_, err := strconv.Atoi(cpuLimit)
		if err != nil {
			// Expand to a set of CPU identifiers and get the pinning map.
			_, _, _, pins, err := vm.cpuTopology(cpuLimit)
			if err != nil {
				op.Done(err)
				return err
			}

			// Get the list of PIDs from the VM.
			pids, err := monitor.GetCPUs()
			if err != nil {
				op.Done(err)
				return err
			}

			// Confirm nothing weird is going on.
			if len(pins) != len(pids) {
				return fmt.Errorf("QEMU has less vCPUs than configured")
			}

			for i, pid := range pids {
				set := unix.CPUSet{}
				set.Set(int(pins[uint64(i)]))

				// Apply the pin.
				err := unix.SchedSetaffinity(pid, &set)
				if err != nil {
					op.Done(err)
					return err
				}
			}
		}
	}

	// Start the VM.
	err = monitor.Start()
	if err != nil {
		op.Done(err)
		return err
	}

	// Database updates
	err = vm.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Record current state
		err = tx.ContainerSetState(vm.id, "RUNNING")
		if err != nil {
			err = errors.Wrap(err, "Error updating instance state")
			op.Done(err)
			return err
		}

		// Update time instance last started time
		err = tx.ContainerLastUsedUpdate(vm.id, time.Now().UTC())
		if err != nil {
			err = errors.Wrap(err, "Error updating instance last used")
			op.Done(err)
			return err
		}

		return nil
	})
	if err != nil {
		op.Done(err)
		return err
	}

	revert.Success()
	vm.state.Events.SendLifecycle(vm.project, "virtual-machine-started", fmt.Sprintf("/1.0/virtual-machines/%s", vm.name), nil)
	return nil
}

// openUnixSocket connects to a UNIX socket and returns the connection.
func (vm *qemu) openUnixSocket(sockPath string) (*net.UnixConn, error) {
	addr, err := net.ResolveUnixAddr("unix", sockPath)
	if err != nil {
		return nil, err
	}

	c, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (vm *qemu) setupNvram() error {
	// No UEFI nvram for ppc64le.
	if vm.architecture == osarch.ARCH_64BIT_POWERPC_LITTLE_ENDIAN {
		return nil
	}

	// Mount the instance's config volume.
	ourMount, err := vm.mount()
	if err != nil {
		return err
	}

	if ourMount {
		defer vm.unmount()
	}

	srcOvmfFile := filepath.Join(vm.ovmfPath(), "OVMF_VARS.fd")
	if vm.expandedConfig["security.secureboot"] == "" || shared.IsTrue(vm.expandedConfig["security.secureboot"]) {
		srcOvmfFile = filepath.Join(vm.ovmfPath(), "OVMF_VARS.ms.fd")
	}

	if !shared.PathExists(srcOvmfFile) {
		return fmt.Errorf("Required EFI firmware settings file missing: %s", srcOvmfFile)
	}

	os.Remove(vm.getNvramPath())
	err = shared.FileCopy(srcOvmfFile, vm.getNvramPath())
	if err != nil {
		return err
	}

	return nil
}

func (vm *qemu) qemuArchConfig() (string, error) {
	if vm.architecture == osarch.ARCH_64BIT_INTEL_X86 {
		return "qemu-system-x86_64", nil
	} else if vm.architecture == osarch.ARCH_64BIT_ARMV8_LITTLE_ENDIAN {
		return "qemu-system-aarch64", nil
	} else if vm.architecture == osarch.ARCH_64BIT_POWERPC_LITTLE_ENDIAN {
		return "qemu-system-ppc64", nil
	}

	return "", fmt.Errorf("Architecture isn't supported for virtual machines")
}

// deviceVolatileGetFunc returns a function that retrieves a named device's volatile config and
// removes its device prefix from the keys.
func (vm *qemu) deviceVolatileGetFunc(devName string) func() map[string]string {
	return func() map[string]string {
		volatile := make(map[string]string)
		prefix := fmt.Sprintf("volatile.%s.", devName)
		for k, v := range vm.localConfig {
			if strings.HasPrefix(k, prefix) {
				volatile[strings.TrimPrefix(k, prefix)] = v
			}
		}
		return volatile
	}
}

// deviceVolatileSetFunc returns a function that can be called to save a named device's volatile
// config using keys that do not have the device's name prefixed.
func (vm *qemu) deviceVolatileSetFunc(devName string) func(save map[string]string) error {
	return func(save map[string]string) error {
		volatileSave := make(map[string]string)
		for k, v := range save {
			volatileSave[fmt.Sprintf("volatile.%s.%s", devName, k)] = v
		}

		return vm.VolatileSet(volatileSave)
	}
}

// RegisterDevices is not used by VMs.
func (vm *qemu) RegisterDevices() {
	return
}

// SaveConfigFile is not used by VMs.
func (vm *qemu) SaveConfigFile() error {
	return instance.ErrNotImplemented
}

// OnHook is the top-level hook handler.
func (vm *qemu) OnHook(hookName string, args map[string]string) error {
	return instance.ErrNotImplemented
}

// deviceLoad instantiates and validates a new device and returns it along with enriched config.
func (vm *qemu) deviceLoad(deviceName string, rawConfig deviceConfig.Device) (device.Device, deviceConfig.Device, error) {
	var configCopy deviceConfig.Device
	var err error

	// Create copy of config and load some fields from volatile if device is nic or infiniband.
	if shared.StringInSlice(rawConfig["type"], []string{"nic", "infiniband"}) {
		configCopy, err = vm.FillNetworkDevice(deviceName, rawConfig)
		if err != nil {
			return nil, nil, err
		}
	} else {
		// Othewise copy the config so it cannot be modified by device.
		configCopy = rawConfig.Clone()
	}

	d, err := device.New(vm, vm.state, deviceName, configCopy, vm.deviceVolatileGetFunc(deviceName), vm.deviceVolatileSetFunc(deviceName))

	// Return device and config copy even if error occurs as caller may still use device.
	return d, configCopy, err
}

// deviceStart loads a new device and calls its Start() function. After processing the runtime
// config returned from Start(), it also runs the device's Register() function irrespective of
// whether the instance is running or not.
func (vm *qemu) deviceStart(deviceName string, rawConfig deviceConfig.Device, isRunning bool) (*deviceConfig.RunConfig, error) {
	d, _, err := vm.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return nil, err
	}

	if canHotPlug, _ := d.CanHotPlug(); isRunning && !canHotPlug {
		return nil, fmt.Errorf("Device cannot be started when instance is running")
	}

	runConf, err := d.Start()
	if err != nil {
		return nil, err
	}

	return runConf, nil
}

// deviceStop loads a new device and calls its Stop() function.
func (vm *qemu) deviceStop(deviceName string, rawConfig deviceConfig.Device) error {
	d, _, err := vm.deviceLoad(deviceName, rawConfig)

	// If deviceLoad fails with unsupported device type then return.
	if err == device.ErrUnsupportedDevType {
		return err
	}

	// If deviceLoad fails for any other reason then just log the error and proceed, as in the
	// scenario that a new version of LXD has additional validation restrictions than older
	// versions we still need to allow previously valid devices to be stopped.
	if err != nil {
		// If there is no device returned, then we cannot proceed, so return as error.
		if d == nil {
			return fmt.Errorf("Device stop validation failed for '%s': %v", deviceName, err)

		}

		logger.Errorf("Device stop validation failed for '%s': %v", deviceName, err)
	}

	canHotPlug, _ := d.CanHotPlug()

	if vm.IsRunning() && !canHotPlug {
		return fmt.Errorf("Device cannot be stopped when instance is running")
	}

	runConf, err := d.Stop()
	if err != nil {
		return err
	}

	if runConf != nil {
		// Run post stop hooks irrespective of run state of instance.
		err = vm.runHooks(runConf.PostHooks)
		if err != nil {
			return err
		}
	}

	return nil
}

// runHooks executes the callback functions returned from a function.
func (vm *qemu) runHooks(hooks []func() error) error {
	// Run any post start hooks.
	if len(hooks) > 0 {
		for _, hook := range hooks {
			err := hook()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (vm *qemu) getMonitorPath() string {
	return filepath.Join(vm.LogPath(), "qemu.monitor")
}

func (vm *qemu) getNvramPath() string {
	return filepath.Join(vm.Path(), "qemu.nvram")
}

// generateConfigShare generates the config share directory that will be exported to the VM via
// a 9P share. Due to the unknown size of templates inside the images this directory is created
// inside the VM's config volume so that it can be restricted by quota.
func (vm *qemu) generateConfigShare() error {
	// Mount the instance's config volume if needed.
	ourMount, err := vm.mount()
	if err != nil {
		return err
	}

	if ourMount {
		defer vm.unmount()
	}

	configDrivePath := filepath.Join(vm.Path(), "config")

	// Create config drive dir.
	os.RemoveAll(configDrivePath)
	err = os.MkdirAll(configDrivePath, 0500)
	if err != nil {
		return err
	}

	// Generate the cloud-init config.
	err = os.MkdirAll(filepath.Join(configDrivePath, "cloud-init"), 0500)
	if err != nil {
		return err
	}

	if vm.ExpandedConfig()["user.user-data"] != "" {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "user-data"), []byte(vm.ExpandedConfig()["user.user-data"]), 0400)
		if err != nil {
			return err
		}
	} else {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "user-data"), []byte("#cloud-config\n"), 0400)
		if err != nil {
			return err
		}
	}

	if vm.ExpandedConfig()["user.vendor-data"] != "" {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "vendor-data"), []byte(vm.ExpandedConfig()["user.vendor-data"]), 0400)
		if err != nil {
			return err
		}
	} else {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "vendor-data"), []byte("#cloud-config\n"), 0400)
		if err != nil {
			return err
		}
	}

	if vm.ExpandedConfig()["user.network-config"] != "" {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "network-config"), []byte(vm.ExpandedConfig()["user.network-config"]), 0400)
		if err != nil {
			return err
		}
	} else {
		os.Remove(filepath.Join(configDrivePath, "cloud-init", "network-config"))
	}

	// Append any user.meta-data to our predefined meta-data config.
	err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "meta-data"), []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n%s\n", vm.Name(), vm.Name(), vm.ExpandedConfig()["user.meta-data"])), 0400)
	if err != nil {
		return err
	}

	// Add the VM agent.
	path, err := exec.LookPath("lxd-agent")
	if err != nil {
		logger.Warnf("lxd-agent not found, skipping its inclusion in the VM config drive: %v", err)
	} else {
		// Install agent into config drive dir if found.
		err = shared.FileCopy(path, filepath.Join(configDrivePath, "lxd-agent"))
		if err != nil {
			return err
		}

		err = os.Chmod(filepath.Join(configDrivePath, "lxd-agent"), 0500)
		if err != nil {
			return err
		}

		err = os.Chown(filepath.Join(configDrivePath, "lxd-agent"), 0, 0)
		if err != nil {
			return err
		}
	}

	agentCert, agentKey, clientCert, _, err := vm.generateAgentCert()
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "server.crt"), []byte(clientCert), 0400)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "agent.crt"), []byte(agentCert), 0400)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "agent.key"), []byte(agentKey), 0400)
	if err != nil {
		return err
	}

	// Systemd units.
	err = os.MkdirAll(filepath.Join(configDrivePath, "systemd"), 0500)
	if err != nil {
		return err
	}

	lxdAgentServiceUnit := `[Unit]
Description=LXD - agent
Documentation=https://linuxcontainers.org/lxd
ConditionPathExists=/dev/virtio-ports/org.linuxcontainers.lxd
Requires=lxd-agent-9p.service
After=lxd-agent-9p.service
Before=cloud-init.target

[Service]
Type=simple
WorkingDirectory=/run/lxd_config/9p
ExecStart=/run/lxd_config/9p/lxd-agent

[Install]
WantedBy=multi-user.target
`

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "systemd", "lxd-agent.service"), []byte(lxdAgentServiceUnit), 0400)
	if err != nil {
		return err
	}

	lxdConfigShareMountUnit := `[Unit]
Description=LXD - agent - 9p mount
Documentation=https://linuxcontainers.org/lxd
ConditionPathExists=/dev/virtio-ports/org.linuxcontainers.lxd

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=-/sbin/modprobe 9pnet_virtio
ExecStartPre=/bin/mkdir -p /run/lxd_config/9p
ExecStartPre=/bin/chmod 0700 /run/lxd_config/
ExecStart=/bin/mount -t 9p config /run/lxd_config/9p -o access=0,trans=virtio

[Install]
WantedBy=multi-user.target
`

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "systemd", "lxd-agent-9p.service"), []byte(lxdConfigShareMountUnit), 0400)
	if err != nil {
		return err
	}

	// Install script for manual installs.
	lxdConfigShareInstall := `#!/bin/sh
if [ ! -e "systemd" ] || [ ! -e "lxd-agent" ]; then
    echo "This script must be run from within the 9p mount"
    exit 1
fi

if [ ! -e "/lib/systemd/system" ]; then
    echo "This script only works on systemd systems"
    exit 1
fi

cp systemd/lxd-agent.service /lib/systemd/system/
cp systemd/lxd-agent-9p.service /lib/systemd/system/
systemctl daemon-reload
systemctl enable lxd-agent.service lxd-agent-9p.service

echo ""
echo "LXD agent has been installed, reboot to confirm setup."
echo "To start it now, unmount this filesystem and run: systemctl start lxd-agent-9p lxd-agent"
`

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "install.sh"), []byte(lxdConfigShareInstall), 0700)
	if err != nil {
		return err
	}

	// Templated files.
	err = os.MkdirAll(filepath.Join(configDrivePath, "files"), 0500)
	if err != nil {
		return err
	}

	// Template anything that needs templating.
	key := "volatile.apply_template"
	if vm.localConfig[key] != "" {
		// Run any template that needs running.
		err = vm.templateApplyNow(vm.localConfig[key], filepath.Join(configDrivePath, "files"))
		if err != nil {
			return err
		}

		// Remove the volatile key from the DB.
		err := vm.state.Cluster.ContainerConfigRemove(vm.id, key)
		if err != nil {
			return err
		}
	}

	err = vm.templateApplyNow("start", filepath.Join(configDrivePath, "files"))
	if err != nil {
		return err
	}

	// Copy the template metadata itself too.
	metaPath := filepath.Join(vm.Path(), "metadata.yaml")
	if shared.PathExists(metaPath) {
		err = shared.FileCopy(metaPath, filepath.Join(configDrivePath, "files/metadata.yaml"))
		if err != nil {
			return err
		}
	}

	return nil
}

func (vm *qemu) templateApplyNow(trigger string, path string) error {
	// If there's no metadata, just return.
	fname := filepath.Join(vm.Path(), "metadata.yaml")
	if !shared.PathExists(fname) {
		return nil
	}

	// Parse the metadata.
	content, err := ioutil.ReadFile(fname)
	if err != nil {
		return errors.Wrap(err, "Failed to read metadata")
	}

	metadata := new(api.ImageMetadata)
	err = yaml.Unmarshal(content, &metadata)
	if err != nil {
		return errors.Wrapf(err, "Could not parse %s", fname)
	}

	// Figure out the instance architecture.
	arch, err := osarch.ArchitectureName(vm.architecture)
	if err != nil {
		arch, err = osarch.ArchitectureName(vm.state.OS.Architectures[0])
		if err != nil {
			return errors.Wrap(err, "Failed to detect system architecture")
		}
	}

	// Generate the container metadata.
	instanceMeta := make(map[string]string)
	instanceMeta["name"] = vm.name
	instanceMeta["architecture"] = arch

	if vm.ephemeral {
		instanceMeta["ephemeral"] = "true"
	} else {
		instanceMeta["ephemeral"] = "false"
	}

	// Go through the templates.
	for tplPath, tpl := range metadata.Templates {
		var w *os.File

		// Check if the template should be applied now.
		found := false
		for _, tplTrigger := range tpl.When {
			if tplTrigger == trigger {
				found = true
				break
			}
		}

		if !found {
			continue
		}

		// Create the file itself.
		w, err = os.Create(filepath.Join(path, fmt.Sprintf("%s.out", tpl.Template)))
		if err != nil {
			return err
		}

		// Fix ownership and mode.
		w.Chmod(0644)
		defer w.Close()

		// Read the template.
		tplString, err := ioutil.ReadFile(filepath.Join(vm.TemplatesPath(), tpl.Template))
		if err != nil {
			return errors.Wrap(err, "Failed to read template file")
		}

		// Restrict filesystem access to within the container's rootfs.
		tplSet := pongo2.NewSet(fmt.Sprintf("%s-%s", vm.name, tpl.Template), pongoTemplate.ChrootLoader{Path: vm.TemplatesPath()})
		tplRender, err := tplSet.FromString("{% autoescape off %}" + string(tplString) + "{% endautoescape %}")
		if err != nil {
			return errors.Wrap(err, "Failed to render template")
		}

		configGet := func(confKey, confDefault *pongo2.Value) *pongo2.Value {
			val, ok := vm.expandedConfig[confKey.String()]
			if !ok {
				return confDefault
			}

			return pongo2.AsValue(strings.TrimRight(val, "\r\n"))
		}

		// Render the template.
		tplRender.ExecuteWriter(pongo2.Context{"trigger": trigger,
			"path":       tplPath,
			"instance":   instanceMeta,
			"container":  instanceMeta, // FIXME: remove once most images have moved away.
			"config":     vm.expandedConfig,
			"devices":    vm.expandedDevices,
			"properties": tpl.Properties,
			"config_get": configGet}, w)
	}

	return nil
}

// deviceBootPriorities returns a map keyed on device name containing the boot index to use.
// Qemu tries to boot devices in order of boot index (lowest first).
func (vm *qemu) deviceBootPriorities() (map[string]int, error) {
	type devicePrios struct {
		Name     string
		BootPrio uint32
	}

	devices := []devicePrios{}

	for devName, devConf := range vm.expandedDevices {
		if devConf["type"] != "disk" && devConf["type"] != "nic" {
			continue
		}

		bootPrio := uint32(0) // Default to lowest priority.
		if devConf["boot.priority"] != "" {
			prio, err := strconv.ParseInt(devConf["boot.priority"], 10, 32)
			if err != nil {
				return nil, errors.Wrapf(err, "Invalid boot.priority for device %q", devName)
			}
			bootPrio = uint32(prio)
		} else if devConf["path"] == "/" {
			bootPrio = 1 // Set boot priority of root disk higher than any device without a boot prio.
		}

		devices = append(devices, devicePrios{Name: devName, BootPrio: bootPrio})
	}

	sort.SliceStable(devices, func(i, j int) bool { return devices[i].BootPrio > devices[j].BootPrio })

	sortedDevs := make(map[string]int, len(devices))
	for bootIndex, dev := range devices {
		sortedDevs[dev.Name] = bootIndex
	}

	return sortedDevs, nil
}

// generateQemuConfigFile writes the qemu config file and returns its location.
// It writes the config file inside the VM's log path.
func (vm *qemu) generateQemuConfigFile(devConfs []*deviceConfig.RunConfig, fdFiles *[]string) (string, error) {
	var sb *strings.Builder = &strings.Builder{}

	err := qemuBase.Execute(sb, map[string]interface{}{
		"architecture":     vm.architectureName,
		"ringbufSizeBytes": qmp.RingbufSize,
	})
	if err != nil {
		return "", err
	}

	// Now add the dynamic parts of the config.
	err = vm.addMemoryConfig(sb)
	if err != nil {
		return "", err
	}

	err = vm.addCPUConfig(sb)
	if err != nil {
		return "", err
	}

	err = vm.addFirmwareConfig(sb)
	if err != nil {
		return "", err
	}

	err = vm.addVsockConfig(sb)
	if err != nil {
		return "", err
	}

	err = vm.addMonitorConfig(sb)
	if err != nil {
		return "", err
	}

	err = vm.addConfDriveConfig(sb)
	if err != nil {
		return "", err
	}

	nicIndex := 0
	bootIndexes, err := vm.deviceBootPriorities()
	if err != nil {
		return "", errors.Wrap(err, "Error calculating boot indexes")
	}

	// Record the mounts we are going to do inside the VM using the agent.
	agentMounts := []instancetype.VMAgentMount{}

	for _, runConf := range devConfs {
		// Add drive devices.
		if len(runConf.Mounts) > 0 {
			for _, drive := range runConf.Mounts {
				if drive.TargetPath == "/" {
					err = vm.addRootDriveConfig(sb, bootIndexes, drive)
				} else if drive.FSType == "9p" {
					err = vm.addDriveDirConfig(sb, fdFiles, &agentMounts, drive)
				} else {
					err = vm.addDriveConfig(sb, bootIndexes, drive)
				}
				if err != nil {
					return "", err
				}
			}
		}

		// Add network device.
		if len(runConf.NetworkInterface) > 0 {
			err = vm.addNetDevConfig(sb, nicIndex, bootIndexes, runConf.NetworkInterface, fdFiles)
			if err != nil {
				return "", err
			}
			nicIndex++
		}
	}

	// Write the agent mount config.
	agentMountJSON, err := json.Marshal(agentMounts)
	if err != nil {
		return "", errors.Wrapf(err, "Failed marshalling agent mounts to JSON")
	}

	agentMountFile := filepath.Join(vm.Path(), "config", "agent-mounts.json")
	err = ioutil.WriteFile(agentMountFile, agentMountJSON, 0400)
	if err != nil {
		return "", errors.Wrapf(err, "Failed writing agent mounts file")
	}

	// Write the config file to disk.
	configPath := filepath.Join(vm.LogPath(), "qemu.conf")
	return configPath, ioutil.WriteFile(configPath, []byte(sb.String()), 0640)
}

// addMemoryConfig adds the qemu config required for setting the size of the VM's memory.
func (vm *qemu) addMemoryConfig(sb *strings.Builder) error {
	// Configure memory limit.
	memSize := vm.expandedConfig["limits.memory"]
	if memSize == "" {
		memSize = "1GiB" // Default to 1GiB if no memory limit specified.
	}

	memSizeBytes, err := units.ParseByteSizeString(memSize)
	if err != nil {
		return fmt.Errorf("limits.memory invalid: %v", err)
	}

	return qemuMemory.Execute(sb, map[string]interface{}{
		"architecture": vm.architectureName,
		"memSizeBytes": memSizeBytes,
	})
}

// addVsockConfig adds the qemu config required for setting up the host->VM vsock socket.
func (vm *qemu) addVsockConfig(sb *strings.Builder) error {
	return qemuVsock.Execute(sb, map[string]interface{}{
		"architecture": vm.architectureName,
		"vsockID":      vm.vsockID(),
	})
}

// addCPUConfig adds the qemu config required for setting the number of virtualised CPUs.
func (vm *qemu) addCPUConfig(sb *strings.Builder) error {
	// Default to a single core.
	cpus := vm.expandedConfig["limits.cpu"]
	if cpus == "" {
		cpus = "1"
	}

	ctx := map[string]interface{}{
		"architecture": vm.architectureName,
	}

	cpuCount, err := strconv.Atoi(cpus)
	if err == nil {
		// If not pinning, default to exposing cores.
		ctx["cpuCount"] = cpuCount
		ctx["cpuSockets"] = 1
		ctx["cpuCores"] = cpuCount
		ctx["cpuThreads"] = 1
	} else {
		// Expand to a set of CPU identifiers and get the pinning map.
		nrSockets, nrCores, nrThreads, vcpus, err := vm.cpuTopology(cpus)
		if err != nil {
			return err
		}

		ctx["cpuCount"] = len(vcpus)
		ctx["cpuSockets"] = nrSockets
		ctx["cpuCores"] = nrCores
		ctx["cpuThreads"] = nrThreads
	}

	return qemuCPU.Execute(sb, ctx)
}

// addMonitorConfig adds the qemu config required for setting up the host side VM monitor device.
func (vm *qemu) addMonitorConfig(sb *strings.Builder) error {
	return qemuControlSocket.Execute(sb, map[string]interface{}{
		"architecture": vm.architectureName,
		"path":         vm.getMonitorPath(),
	})
}

// addFirmwareConfig adds the qemu config required for adding a secure boot compatible EFI firmware.
func (vm *qemu) addFirmwareConfig(sb *strings.Builder) error {
	// No UEFI nvram for ppc64le.
	if vm.architecture == osarch.ARCH_64BIT_POWERPC_LITTLE_ENDIAN {
		return nil
	}

	return qemuDriveFirmware.Execute(sb, map[string]interface{}{
		"architecture": vm.architectureName,
		"roPath":       filepath.Join(vm.ovmfPath(), "OVMF_CODE.fd"),
		"nvramPath":    vm.getNvramPath(),
	})
}

// addConfDriveConfig adds the qemu config required for adding the config drive.
func (vm *qemu) addConfDriveConfig(sb *strings.Builder) error {
	return qemuDriveConfig.Execute(sb, map[string]interface{}{
		"architecture": vm.architectureName,
		"path":         filepath.Join(vm.Path(), "config"),
	})
}

// addFileDescriptor adds a file path to the list of files to open and pass file descriptor to qemu.
// Returns the file descriptor number that qemu will receive.
func (vm *qemu) addFileDescriptor(fdFiles *[]string, filePath string) int {
	// Append the tap device file path to the list of files to be opened and passed to qemu.
	*fdFiles = append(*fdFiles, filePath)
	return 2 + len(*fdFiles) // Use 2+fdFiles count, as first user file descriptor is 3.
}

// addRootDriveConfig adds the qemu config required for adding the root drive.
func (vm *qemu) addRootDriveConfig(sb *strings.Builder, bootIndexes map[string]int, rootDriveConf deviceConfig.MountEntryItem) error {
	if rootDriveConf.TargetPath != "/" {
		return fmt.Errorf("Non-root drive config supplied")
	}

	pool, err := vm.getStoragePool()
	if err != nil {
		return err
	}

	rootDrivePath, err := pool.GetInstanceDisk(vm)
	if err != nil {
		return err
	}

	// Generate a new device config with the root device path expanded.
	driveConf := deviceConfig.MountEntryItem{
		DevName: rootDriveConf.DevName,
		DevPath: rootDrivePath,
	}

	// If the storage pool is on ZFS and backed by a loop file and we can't use DirectIO, then resort to
	// unsafe async I/O to avoid kernel hangs when running ZFS storage pools in an image file on another FS.
	driverInfo := pool.Driver().Info()
	driverConf := pool.Driver().Config()
	if driverInfo.Name == "zfs" && !driverInfo.DirectIO && shared.PathExists(driverConf["source"]) && !shared.IsBlockdevPath(driverConf["source"]) {
		driveConf.Opts = append(driveConf.Opts, qemuUnsafeIO)
	}

	return vm.addDriveConfig(sb, bootIndexes, driveConf)
}

// addDriveDirConfig adds the qemu config required for adding a supplementary drive directory share.
func (vm *qemu) addDriveDirConfig(sb *strings.Builder, fdFiles *[]string, agentMounts *[]instancetype.VMAgentMount, driveConf deviceConfig.MountEntryItem) error {
	mountTag := fmt.Sprintf("lxd_%s", driveConf.DevName)

	agentMount := instancetype.VMAgentMount{
		Source: mountTag,
		Target: driveConf.TargetPath,
		FSType: driveConf.FSType,
	}

	// Indicate to agent to mount this readonly. Note: This is purely to indicate to VM guest that this is
	// readonly, it should *not* be used as a security measure, as the VM guest could remount it R/W.
	if shared.StringInSlice("ro", driveConf.Opts) {
		agentMount.Options = append(agentMount.Options, "ro")
	}

	// Record the 9p mount for the agent.
	*agentMounts = append(*agentMounts, agentMount)

	// For read only shares, do not use proxy.
	if shared.StringInSlice("ro", driveConf.Opts) {
		return qemuDriveDir.Execute(sb, map[string]interface{}{
			"devName":  driveConf.DevName,
			"mountTag": mountTag,
			"path":     driveConf.DevPath,
			"readonly": true,
		})
	}

	// Only use proxy for writable shares.
	proxyFD := vm.addFileDescriptor(fdFiles, driveConf.DevPath)
	return qemuDriveDir.Execute(sb, map[string]interface{}{
		"devName":  driveConf.DevName,
		"mountTag": mountTag,
		"proxyFD":  proxyFD,
		"readonly": false,
	})
}

// addDriveConfig adds the qemu config required for adding a supplementary drive.
func (vm *qemu) addDriveConfig(sb *strings.Builder, bootIndexes map[string]int, driveConf deviceConfig.MountEntryItem) error {
	// Use native kernel async IO and O_DIRECT by default.
	aioMode := "native"
	cacheMode := "none" // Bypass host cache, use O_DIRECT semantics.

	// If drive config indicates we need to use unsafe I/O then use it.
	if shared.StringInSlice(qemuUnsafeIO, driveConf.Opts) {
		logger.Warnf("Using unsafe cache I/O with %s", driveConf.DevPath)
		aioMode = "threads"
		cacheMode = "unsafe" // Use host cache, but ignore all sync requests from guest.
	} else if shared.PathExists(driveConf.DevPath) && !shared.IsBlockdevPath(driveConf.DevPath) {
		// Disk dev path is a file, check whether it is located on a ZFS filesystem.
		fsType, err := util.FilesystemDetect(driveConf.DevPath)
		if err != nil {
			return errors.Wrapf(err, "Failed detecting filesystem type of %q", driveConf.DevPath)
		}

		// If FS is ZFS, avoid using direct I/O and use host page cache only.
		if fsType == "zfs" {
			if driveConf.FSType != "iso9660" {
				logger.Warnf("Using writeback cache I/O with %s", driveConf.DevPath)
			}
			aioMode = "threads"
			cacheMode = "writeback" // Use host cache, with neither O_DSYNC nor O_DIRECT semantics.
		}
	}

	return qemuDrive.Execute(sb, map[string]interface{}{
		"devName":   driveConf.DevName,
		"devPath":   driveConf.DevPath,
		"bootIndex": bootIndexes[driveConf.DevName],
		"cacheMode": cacheMode,
		"aioMode":   aioMode,
	})
}

// addNetDevConfig adds the qemu config required for adding a network device.
func (vm *qemu) addNetDevConfig(sb *strings.Builder, nicIndex int, bootIndexes map[string]int, nicConfig []deviceConfig.RunConfigItem, fdFiles *[]string) error {
	var devName, nicName, devHwaddr, pciSlotName string
	for _, nicItem := range nicConfig {
		if nicItem.Key == "devName" {
			devName = nicItem.Value
		} else if nicItem.Key == "link" {
			nicName = nicItem.Value
		} else if nicItem.Key == "hwaddr" {
			devHwaddr = nicItem.Value
		} else if nicItem.Key == "pciSlotName" {
			pciSlotName = nicItem.Value
		}
	}

	var tpl *template.Template
	tplFields := map[string]interface{}{
		"architecture": vm.architectureName,
		"devName":      devName,
		"devHwaddr":    devHwaddr,
		"bootIndex":    bootIndexes[devName],
		"chassisIndex": 5 + nicIndex,
		"portIndex":    14 + nicIndex,
		"pcieAddr":     4 + nicIndex,
	}

	// Detect MACVTAP interface types and figure out which tap device is being used.
	// This is so we can open a file handle to the tap device and pass it to the qemu process.
	if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/macvtap", nicName)) {
		content, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/ifindex", nicName))
		if err != nil {
			return errors.Wrapf(err, "Error getting tap device ifindex")
		}

		ifindex, err := strconv.Atoi(strings.TrimSpace(string(content)))
		if err != nil {
			return errors.Wrapf(err, "Error parsing tap device ifindex")
		}

		// Append the tap device file path to the list of files to be opened and passed to qemu.
		tplFields["tapFD"] = vm.addFileDescriptor(fdFiles, fmt.Sprintf("/dev/tap%d", ifindex))
		tpl = qemuNetdevTapFD
	} else if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/tun_flags", nicName)) {
		// Detect TAP (via TUN driver) device.
		tplFields["ifName"] = nicName
		tpl = qemuNetDevTapTun
	} else if pciSlotName != "" {
		// Detect physical passthrough device.
		tplFields["pciSlotName"] = pciSlotName
		tpl = qemuNetdevPhysical
	}

	if tpl != nil {
		return tpl.Execute(sb, tplFields)
	}

	return fmt.Errorf("Unrecognised device type")
}

// pidFilePath returns the path where the qemu process should write its PID.
func (vm *qemu) pidFilePath() string {
	return filepath.Join(vm.LogPath(), "qemu.pid")
}

// pid gets the PID of the running qemu process.
func (vm *qemu) pid() (int, error) {
	pidStr, err := ioutil.ReadFile(vm.pidFilePath())
	if os.IsNotExist(err) {
		return 0, nil
	}

	if err != nil {
		return -1, err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidStr)))
	if err != nil {
		return -1, err
	}

	return pid, nil
}

// Stop stops the VM.
func (vm *qemu) Stop(stateful bool) error {
	// Check that we're not already stopped.
	if !vm.IsRunning() {
		return fmt.Errorf("The instance is already stopped")
	}

	// Check that no stateful stop was requested.
	if stateful {
		return fmt.Errorf("Stateful stop isn't supported for VMs at this time")
	}

	// Setup a new operation.
	op, err := operationlock.Create(vm.id, "stop", false, true)
	if err != nil {
		return err
	}

	// Connect to the monitor.
	monitor, err := qmp.Connect(vm.getMonitorPath(), vm.getMonitorEventHandler())
	if err != nil {
		// If we fail to connect, it's most likely because the VM is already off.
		op.Done(nil)
		return nil
	}

	// Get the wait channel.
	chDisconnect, err := monitor.Wait()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			op.Done(nil)
			return nil
		}

		op.Done(err)
		return err
	}

	// Send the quit command.
	err = monitor.Quit()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			op.Done(nil)
			return nil
		}

		op.Done(err)
		return err
	}

	// Wait for QEMU to exit (can take a while if pending I/O).
	<-chDisconnect

	// Wait for OnStop.
	err = op.Wait()
	if err != nil && vm.IsRunning() {
		return err
	}

	vm.state.Events.SendLifecycle(vm.project, "virtual-machine-stopped", fmt.Sprintf("/1.0/virtual-machines/%s", vm.name), nil)
	return nil
}

// Unfreeze restores the instance to running.
func (vm *qemu) Unfreeze() error {
	// Connect to the monitor.
	monitor, err := qmp.Connect(vm.getMonitorPath(), vm.getMonitorEventHandler())
	if err != nil {
		return err
	}

	// Send the cont command.
	err = monitor.Start()
	if err != nil {
		return err
	}

	return nil
}

// IsPrivileged does not apply to virtual machines. Always returns false.
func (vm *qemu) IsPrivileged() bool {
	return false
}

// Restore restores an instance snapshot.
func (vm *qemu) Restore(source instance.Instance, stateful bool) error {
	if stateful {
		return fmt.Errorf("Stateful snapshots of VMs aren't supported yet")
	}

	var ctxMap log.Ctx

	// Load the storage driver.
	pool, err := storagePools.GetPoolByInstance(vm.state, vm)
	if err != nil {
		return err
	}

	// Ensure that storage is mounted for backup.yaml updates.
	ourStart, err := pool.MountInstance(vm, nil)
	if err != nil {
		return err
	}
	if ourStart {
		defer pool.UnmountInstance(vm, nil)
	}

	// Stop the instance.
	wasRunning := false
	if vm.IsRunning() {
		wasRunning = true

		ephemeral := vm.IsEphemeral()
		if ephemeral {
			// Unset ephemeral flag.
			args := db.InstanceArgs{
				Architecture: vm.Architecture(),
				Config:       vm.LocalConfig(),
				Description:  vm.Description(),
				Devices:      vm.LocalDevices(),
				Ephemeral:    false,
				Profiles:     vm.Profiles(),
				Project:      vm.Project(),
				Type:         vm.Type(),
				Snapshot:     vm.IsSnapshot(),
			}

			err := vm.Update(args, false)
			if err != nil {
				return err
			}

			// On function return, set the flag back on.
			defer func() {
				args.Ephemeral = ephemeral
				vm.Update(args, true)
			}()
		}

		// This will unmount the instance storage.
		err := vm.Stop(false)
		if err != nil {
			return err
		}
	}

	ctxMap = log.Ctx{
		"project":   vm.project,
		"name":      vm.name,
		"created":   vm.creationDate,
		"ephemeral": vm.ephemeral,
		"used":      vm.lastUsedDate,
		"source":    source.Name()}

	logger.Info("Restoring instance", ctxMap)

	// Restore the rootfs.
	err = pool.RestoreInstanceSnapshot(vm, source, nil)
	if err != nil {
		return err
	}

	// Restore the configuration.
	args := db.InstanceArgs{
		Architecture: source.Architecture(),
		Config:       source.LocalConfig(),
		Description:  source.Description(),
		Devices:      source.LocalDevices(),
		Ephemeral:    source.IsEphemeral(),
		Profiles:     source.Profiles(),
		Project:      source.Project(),
		Type:         source.Type(),
		Snapshot:     source.IsSnapshot(),
	}

	err = vm.Update(args, false)
	if err != nil {
		logger.Error("Failed restoring instance configuration", ctxMap)
		return err
	}

	// The old backup file may be out of date (e.g. it doesn't have all the current snapshots of
	// the instance listed); let's write a new one to be safe.
	err = vm.UpdateBackupFile()
	if err != nil {
		return err
	}

	vm.state.Events.SendLifecycle(vm.project, "virtual-machine-snapshot-restored", fmt.Sprintf("/1.0/virtual-machines/%s", vm.name), map[string]interface{}{"snapshot_name": vm.name})

	// Restart the insance.
	if wasRunning {
		logger.Info("Restored instance", ctxMap)
		return vm.Start(false)
	}

	logger.Info("Restored instance", ctxMap)
	return nil
}

// Snapshots returns a list of snapshots.
func (vm *qemu) Snapshots() ([]instance.Instance, error) {
	var snaps []db.Instance

	if vm.IsSnapshot() {
		return []instance.Instance{}, nil
	}

	// Get all the snapshots
	err := vm.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		snaps, err = tx.ContainerGetSnapshotsFull(vm.Project(), vm.name)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Build the snapshot list
	snapshots, err := instance.LoadAllInternal(vm.state, snaps)
	if err != nil {
		return nil, err
	}

	instances := make([]instance.Instance, len(snapshots))
	for k, v := range snapshots {
		instances[k] = instance.Instance(v)
	}

	return instances, nil
}

// Backups returns a list of backups.
func (vm *qemu) Backups() ([]backup.Backup, error) {
	return []backup.Backup{}, nil
}

// Rename the instance.
func (vm *qemu) Rename(newName string) error {
	oldName := vm.Name()
	ctxMap := log.Ctx{
		"project":   vm.project,
		"name":      vm.name,
		"created":   vm.creationDate,
		"ephemeral": vm.ephemeral,
		"used":      vm.lastUsedDate,
		"newname":   newName}

	logger.Info("Renaming instance", ctxMap)

	// Sanity checks.
	err := instance.ValidName(newName, vm.IsSnapshot())
	if err != nil {
		return err
	}

	if vm.IsRunning() {
		return fmt.Errorf("Renaming of running instance not allowed")
	}

	// Clean things up.
	vm.cleanup()

	pool, err := storagePools.GetPoolByInstance(vm.state, vm)
	if err != nil {
		return errors.Wrap(err, "Load instance storage pool")
	}

	if vm.IsSnapshot() {
		_, newSnapName, _ := shared.InstanceGetParentAndSnapshotName(newName)
		err = pool.RenameInstanceSnapshot(vm, newSnapName, nil)
		if err != nil {
			return errors.Wrap(err, "Rename instance snapshot")
		}
	} else {
		err = pool.RenameInstance(vm, newName, nil)
		if err != nil {
			return errors.Wrap(err, "Rename instance")
		}
	}

	if !vm.IsSnapshot() {
		// Rename all the instance snapshot database entries.
		results, err := vm.state.Cluster.ContainerGetSnapshots(vm.project, oldName)
		if err != nil {
			logger.Error("Failed to get instance snapshots", ctxMap)
			return err
		}

		for _, sname := range results {
			// Rename the snapshot.
			oldSnapName := strings.SplitN(sname, shared.SnapshotDelimiter, 2)[1]
			baseSnapName := filepath.Base(sname)
			err := vm.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
				return tx.InstanceSnapshotRename(vm.project, oldName, oldSnapName, baseSnapName)
			})
			if err != nil {
				logger.Error("Failed renaming snapshot", ctxMap)
				return err
			}
		}
	}

	// Rename the instance database entry.
	err = vm.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		if vm.IsSnapshot() {
			oldParts := strings.SplitN(oldName, shared.SnapshotDelimiter, 2)
			newParts := strings.SplitN(newName, shared.SnapshotDelimiter, 2)
			return tx.InstanceSnapshotRename(vm.project, oldParts[0], oldParts[1], newParts[1])
		}

		return tx.InstanceRename(vm.project, oldName, newName)
	})
	if err != nil {
		logger.Error("Failed renaming instance", ctxMap)
		return err
	}

	// Rename the logging path.
	os.RemoveAll(shared.LogPath(newName))
	if shared.PathExists(vm.LogPath()) {
		err := os.Rename(vm.LogPath(), shared.LogPath(newName))
		if err != nil {
			logger.Error("Failed renaming instance", ctxMap)
			return err
		}
	}

	// Rename the MAAS entry.
	if !vm.IsSnapshot() {
		err = vm.maasRename(newName)
		if err != nil {
			return err
		}
	}

	// Rename the backups.
	backups, err := vm.Backups()
	if err != nil {
		return err
	}

	for _, backup := range backups {
		backupName := strings.Split(backup.Name(), "/")[1]
		newName := fmt.Sprintf("%s/%s", newName, backupName)

		err = backup.Rename(newName)
		if err != nil {
			return err
		}
	}

	// Set the new name in the struct.
	vm.name = newName

	// Update lease files.
	network.UpdateDNSMasqStatic(vm.state, "")

	logger.Info("Renamed instance", ctxMap)

	if vm.IsSnapshot() {
		vm.state.Events.SendLifecycle(vm.project, "virtual-machine-snapshot-renamed",
			fmt.Sprintf("/1.0/virtual-machines/%s", oldName), map[string]interface{}{
				"new_name":      newName,
				"snapshot_name": oldName,
			})
	} else {
		vm.state.Events.SendLifecycle(vm.project, "virtual-machine-renamed",
			fmt.Sprintf("/1.0/virtual-machines/%s", oldName), map[string]interface{}{
				"new_name": newName,
			})
	}

	return nil
}

// Update the instance config.
func (vm *qemu) Update(args db.InstanceArgs, userRequested bool) error {
	if vm.IsRunning() {
		return fmt.Errorf("Update whilst running not supported")
	}

	// Set sane defaults for unset keys.
	if args.Project == "" {
		args.Project = project.Default
	}

	if args.Architecture == 0 {
		args.Architecture = vm.architecture
	}

	if args.Config == nil {
		args.Config = map[string]string{}
	}

	if args.Devices == nil {
		args.Devices = deviceConfig.Devices{}
	}

	if args.Profiles == nil {
		args.Profiles = []string{}
	}

	// Validate the new config.
	err := instance.ValidConfig(vm.state.OS, args.Config, false, false)
	if err != nil {
		return errors.Wrap(err, "Invalid config")
	}

	// Validate the new devices without using expanded devices validation (expensive checks disabled).
	err = instance.ValidDevices(vm.state, vm.state.Cluster, vm.Type(), args.Devices, false)
	if err != nil {
		return errors.Wrap(err, "Invalid devices")
	}

	// Validate the new profiles.
	profiles, err := vm.state.Cluster.Profiles(args.Project)
	if err != nil {
		return errors.Wrap(err, "Failed to get profiles")
	}

	checkedProfiles := []string{}
	for _, profile := range args.Profiles {
		if !shared.StringInSlice(profile, profiles) {
			return fmt.Errorf("Requested profile '%s' doesn't exist", profile)
		}

		if shared.StringInSlice(profile, checkedProfiles) {
			return fmt.Errorf("Duplicate profile found in request")
		}

		checkedProfiles = append(checkedProfiles, profile)
	}

	// Validate the new architecture.
	if args.Architecture != 0 {
		_, err = osarch.ArchitectureName(args.Architecture)
		if err != nil {
			return fmt.Errorf("Invalid architecture ID: %s", err)
		}
	}

	// Check that volatile and image keys weren't modified.
	if userRequested {
		for k, v := range args.Config {
			if strings.HasPrefix(k, "volatile.") && vm.localConfig[k] != v {
				return fmt.Errorf("Volatile keys are read-only")
			}

			if strings.HasPrefix(k, "image.") && vm.localConfig[k] != v {
				return fmt.Errorf("Image keys are read-only")
			}
		}

		for k, v := range vm.localConfig {
			if strings.HasPrefix(k, "volatile.") && args.Config[k] != v {
				return fmt.Errorf("Volatile keys are read-only")
			}

			if strings.HasPrefix(k, "image.") && args.Config[k] != v {
				return fmt.Errorf("Image keys are read-only")
			}
		}
	}

	// Get a copy of the old configuration.
	oldDescription := vm.Description()
	oldArchitecture := 0
	err = shared.DeepCopy(&vm.architecture, &oldArchitecture)
	if err != nil {
		return err
	}

	oldEphemeral := false
	err = shared.DeepCopy(&vm.ephemeral, &oldEphemeral)
	if err != nil {
		return err
	}

	oldExpandedDevices := deviceConfig.Devices{}
	err = shared.DeepCopy(&vm.expandedDevices, &oldExpandedDevices)
	if err != nil {
		return err
	}

	oldExpandedConfig := map[string]string{}
	err = shared.DeepCopy(&vm.expandedConfig, &oldExpandedConfig)
	if err != nil {
		return err
	}

	oldLocalDevices := deviceConfig.Devices{}
	err = shared.DeepCopy(&vm.localDevices, &oldLocalDevices)
	if err != nil {
		return err
	}

	oldLocalConfig := map[string]string{}
	err = shared.DeepCopy(&vm.localConfig, &oldLocalConfig)
	if err != nil {
		return err
	}

	oldProfiles := []string{}
	err = shared.DeepCopy(&vm.profiles, &oldProfiles)
	if err != nil {
		return err
	}

	oldExpiryDate := vm.expiryDate

	// Define a function which reverts everything.  Defer this function
	// so that it doesn't need to be explicitly called in every failing
	// return path.  Track whether or not we want to undo the changes
	// using a closure.
	undoChanges := true
	defer func() {
		if undoChanges {
			vm.description = oldDescription
			vm.architecture = oldArchitecture
			vm.ephemeral = oldEphemeral
			vm.expandedConfig = oldExpandedConfig
			vm.expandedDevices = oldExpandedDevices
			vm.localConfig = oldLocalConfig
			vm.localDevices = oldLocalDevices
			vm.profiles = oldProfiles
			vm.expiryDate = oldExpiryDate
		}
	}()

	// Apply the various changes.
	vm.description = args.Description
	vm.architecture = args.Architecture
	vm.ephemeral = args.Ephemeral
	vm.localConfig = args.Config
	vm.localDevices = args.Devices
	vm.profiles = args.Profiles
	vm.expiryDate = args.ExpiryDate

	// Expand the config and refresh the LXC config.
	err = vm.expandConfig(nil)
	if err != nil {
		return errors.Wrap(err, "Expand config")
	}

	err = vm.expandDevices(nil)
	if err != nil {
		return errors.Wrap(err, "Expand devices")
	}

	// Diff the configurations.
	changedConfig := []string{}
	for key := range oldExpandedConfig {
		if oldExpandedConfig[key] != vm.expandedConfig[key] {
			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	for key := range vm.expandedConfig {
		if oldExpandedConfig[key] != vm.expandedConfig[key] {
			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	// Diff the devices.
	removeDevices, addDevices, updateDevices, updateDiff := oldExpandedDevices.Update(vm.expandedDevices, func(oldDevice deviceConfig.Device, newDevice deviceConfig.Device) []string {
		// This function needs to return a list of fields that are excluded from differences
		// between oldDevice and newDevice. The result of this is that as long as the
		// devices are otherwise identical except for the fields returned here, then the
		// device is considered to be being "updated" rather than "added & removed".
		if oldDevice["type"] != newDevice["type"] || oldDevice.NICType() != newDevice.NICType() {
			return []string{} // Device types aren't the same, so this cannot be an update.
		}

		d, err := device.New(vm, vm.state, "", newDevice, nil, nil)
		if err != nil {
			return []string{} // Couldn't create Device, so this cannot be an update.
		}

		_, updateFields := d.CanHotPlug()
		return updateFields
	})

	// Do some validation of the config diff.
	err = instance.ValidConfig(vm.state.OS, vm.expandedConfig, false, true)
	if err != nil {
		return errors.Wrap(err, "Invalid expanded config")
	}

	// Do full expanded validation of the devices diff.
	err = instance.ValidDevices(vm.state, vm.state.Cluster, vm.Type(), vm.expandedDevices, true)
	if err != nil {
		return errors.Wrap(err, "Invalid expanded devices")
	}

	// Use the device interface to apply update changes.
	err = vm.updateDevices(removeDevices, addDevices, updateDevices, oldExpandedDevices)
	if err != nil {
		return err
	}

	// Update MAAS (must run after the MAC addresses have been generated).
	updateMAAS := false
	for _, key := range []string{"maas.subnet.ipv4", "maas.subnet.ipv6", "ipv4.address", "ipv6.address"} {
		if shared.StringInSlice(key, updateDiff) {
			updateMAAS = true
			break
		}
	}

	if !vm.IsSnapshot() && updateMAAS {
		err = vm.maasUpdate(oldExpandedDevices.CloneNative())
		if err != nil {
			return err
		}
	}

	if shared.StringInSlice("security.secureboot", changedConfig) {
		// Re-generate the NVRAM.
		err = vm.setupNvram()
		if err != nil {
			return err
		}
	}

	// Finally, apply the changes to the database.
	err = query.Retry(func() error {
		tx, err := vm.state.Cluster.Begin()
		if err != nil {
			return err
		}

		// Snapshots should update only their descriptions and expiry date.
		if vm.IsSnapshot() {
			err = db.InstanceSnapshotUpdate(tx, vm.id, vm.description, vm.expiryDate)
			if err != nil {
				tx.Rollback()
				return errors.Wrap(err, "Snapshot update")
			}
		} else {
			err = db.ContainerConfigClear(tx, vm.id)
			if err != nil {
				tx.Rollback()
				return err

			}
			err = db.ContainerConfigInsert(tx, vm.id, vm.localConfig)
			if err != nil {
				tx.Rollback()
				return errors.Wrap(err, "Config insert")
			}

			err = db.ContainerProfilesInsert(tx, vm.id, vm.project, vm.profiles)
			if err != nil {
				tx.Rollback()
				return errors.Wrap(err, "Profiles insert")
			}

			err = db.DevicesAdd(tx, "instance", int64(vm.id), vm.localDevices)
			if err != nil {
				tx.Rollback()
				return errors.Wrap(err, "Device add")
			}

			err = db.ContainerUpdate(tx, vm.id, vm.description, vm.architecture, vm.ephemeral, vm.expiryDate)
			if err != nil {
				tx.Rollback()
				return errors.Wrap(err, "Instance update")
			}

		}

		if err := db.TxCommit(tx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "Failed to update database")
	}

	err = vm.UpdateBackupFile()
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "Failed to write backup file")
	}

	// Success, update the closure to mark that the changes should be kept.
	undoChanges = false

	var endpoint string

	if vm.IsSnapshot() {
		parentName, snapName, _ := shared.InstanceGetParentAndSnapshotName(vm.name)
		endpoint = fmt.Sprintf("/1.0/virtual-machines/%s/snapshots/%s", parentName, snapName)
	} else {
		endpoint = fmt.Sprintf("/1.0/virtual-machines/%s", vm.name)
	}

	vm.state.Events.SendLifecycle(vm.project, "virtual-machine-updated", endpoint, nil)
	return nil
}

func (vm *qemu) updateDevices(removeDevices deviceConfig.Devices, addDevices deviceConfig.Devices, updateDevices deviceConfig.Devices, oldExpandedDevices deviceConfig.Devices) error {
	isRunning := vm.IsRunning()

	// Remove devices in reverse order to how they were added.
	for _, dev := range removeDevices.Reversed() {
		if isRunning {
			err := vm.deviceStop(dev.Name, dev.Config)
			if err == device.ErrUnsupportedDevType {
				continue // No point in trying to remove device below.
			} else if err != nil {
				return errors.Wrapf(err, "Failed to stop device %q", dev.Name)
			}
		}

		err := vm.deviceRemove(dev.Name, dev.Config)
		if err != nil && err != device.ErrUnsupportedDevType {
			return errors.Wrapf(err, "Failed to remove device %q", dev.Name)
		}

		// Check whether we are about to add the same device back with updated config and
		// if not, or if the device type has changed, then remove all volatile keys for
		// this device (as its an actual removal or a device type change).
		err = vm.deviceResetVolatile(dev.Name, dev.Config, addDevices[dev.Name])
		if err != nil {
			return errors.Wrapf(err, "Failed to reset volatile data for device %q", dev.Name)
		}
	}

	// Add devices in sorted order, this ensures that device mounts are added in path order.
	for _, dev := range addDevices.Sorted() {
		err := vm.deviceAdd(dev.Name, dev.Config)
		if err == device.ErrUnsupportedDevType {
			continue // No point in trying to start device below.
		} else if err != nil {
			return errors.Wrapf(err, "Failed to add device %q", dev.Name)
		}

		if isRunning {
			_, err := vm.deviceStart(dev.Name, dev.Config, isRunning)
			if err != nil && err != device.ErrUnsupportedDevType {
				return errors.Wrapf(err, "Failed to start device %q", dev.Name)
			}
		}
	}

	for _, dev := range updateDevices.Sorted() {
		err := vm.deviceUpdate(dev.Name, dev.Config, oldExpandedDevices, isRunning)
		if err != nil && err != device.ErrUnsupportedDevType {
			return errors.Wrapf(err, "Failed to update device %q", dev.Name)
		}
	}

	return nil
}

// deviceUpdate loads a new device and calls its Update() function.
func (vm *qemu) deviceUpdate(deviceName string, rawConfig deviceConfig.Device, oldDevices deviceConfig.Devices, isRunning bool) error {
	d, _, err := vm.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return err
	}

	err = d.Update(oldDevices, isRunning)
	if err != nil {
		return err
	}

	return nil
}

// deviceResetVolatile resets a device's volatile data when its removed or updated in such a way
// that it is removed then added immediately afterwards.
func (vm *qemu) deviceResetVolatile(devName string, oldConfig, newConfig deviceConfig.Device) error {
	volatileClear := make(map[string]string)
	devicePrefix := fmt.Sprintf("volatile.%s.", devName)

	// If the device type has changed, remove all old volatile keys.
	// This will occur if the newConfig is empty (i.e the device is actually being removed) or
	// if the device type is being changed but keeping the same name.
	if newConfig["type"] != oldConfig["type"] || newConfig.NICType() != oldConfig.NICType() {
		for k := range vm.localConfig {
			if !strings.HasPrefix(k, devicePrefix) {
				continue
			}

			volatileClear[k] = ""
		}

		return vm.VolatileSet(volatileClear)
	}

	// If the device type remains the same, then just remove any volatile keys that have
	// the same key name present in the new config (i.e the new config is replacing the
	// old volatile key).
	for k := range vm.localConfig {
		if !strings.HasPrefix(k, devicePrefix) {
			continue
		}

		devKey := strings.TrimPrefix(k, devicePrefix)
		if _, found := newConfig[devKey]; found {
			volatileClear[k] = ""
		}
	}

	return vm.VolatileSet(volatileClear)
}

func (vm *qemu) removeUnixDevices() error {
	// Check that we indeed have devices to remove.
	if !shared.PathExists(vm.DevicesPath()) {
		return nil
	}

	// Load the directory listing.
	dents, err := ioutil.ReadDir(vm.DevicesPath())
	if err != nil {
		return err
	}

	// Go through all the unix devices.
	for _, f := range dents {
		// Skip non-Unix devices.
		if !strings.HasPrefix(f.Name(), "forkmknod.unix.") && !strings.HasPrefix(f.Name(), "unix.") && !strings.HasPrefix(f.Name(), "infiniband.unix.") {
			continue
		}

		// Remove the entry
		devicePath := filepath.Join(vm.DevicesPath(), f.Name())
		err := os.Remove(devicePath)
		if err != nil {
			logger.Error("Failed removing unix device", log.Ctx{"err": err, "path": devicePath})
		}
	}

	return nil
}

func (vm *qemu) removeDiskDevices() error {
	// Check that we indeed have devices to remove.vm
	if !shared.PathExists(vm.DevicesPath()) {
		return nil
	}

	// Load the directory listing.
	dents, err := ioutil.ReadDir(vm.DevicesPath())
	if err != nil {
		return err
	}

	// Go through all the unix devices
	for _, f := range dents {
		// Skip non-disk devices
		if !strings.HasPrefix(f.Name(), "disk.") {
			continue
		}

		// Always try to unmount the host side
		_ = unix.Unmount(filepath.Join(vm.DevicesPath(), f.Name()), unix.MNT_DETACH)

		// Remove the entry
		diskPath := filepath.Join(vm.DevicesPath(), f.Name())
		err := os.Remove(diskPath)
		if err != nil {
			logger.Error("Failed to remove disk device path", log.Ctx{"err": err, "path": diskPath})
		}
	}

	return nil
}

func (vm *qemu) cleanup() {
	// Unmount any leftovers
	vm.removeUnixDevices()
	vm.removeDiskDevices()

	// Remove the devices path
	os.Remove(vm.DevicesPath())

	// Remove the shmounts path
	os.RemoveAll(vm.ShmountsPath())
}

// cleanupDevices performs any needed device cleanup steps when instance is stopped.
func (vm *qemu) cleanupDevices() {
	for _, dev := range vm.expandedDevices.Sorted() {
		// Use the device interface if device supports it.
		err := vm.deviceStop(dev.Name, dev.Config)
		if err == device.ErrUnsupportedDevType {
			continue
		} else if err != nil {
			logger.Errorf("Failed to stop device '%s': %v", dev.Name, err)
		}
	}
}

func (vm *qemu) init() error {
	// Compute the expanded config and device list.
	err := vm.expandConfig(nil)
	if err != nil {
		return err
	}

	err = vm.expandDevices(nil)
	if err != nil {
		return err
	}

	return nil
}

// Delete the instance.
func (vm *qemu) Delete() error {
	ctxMap := log.Ctx{
		"project":   vm.project,
		"name":      vm.name,
		"created":   vm.creationDate,
		"ephemeral": vm.ephemeral,
		"used":      vm.lastUsedDate}

	logger.Info("Deleting instance", ctxMap)

	// Check if instance is delete protected.
	if shared.IsTrue(vm.expandedConfig["security.protection.delete"]) && !vm.IsSnapshot() {
		return fmt.Errorf("Instance is protected")
	}

	// Check if we're dealing with "lxd import".
	// TODO consider lxd import detection for VMs.
	isImport := false

	// Attempt to initialize storage interface for the instance.
	pool, err := vm.getStoragePool()
	if err != nil && err != db.ErrNoSuchObject {
		return err
	} else if pool != nil {
		if vm.IsSnapshot() {
			if !isImport {
				// Remove snapshot volume and database record.
				err = pool.DeleteInstanceSnapshot(vm, nil)
				if err != nil {
					return err
				}
			}
		} else {
			// Remove all snapshots by initialising each snapshot as an Instance and
			// calling its Delete function.
			err := instance.DeleteSnapshots(vm.state, vm.Project(), vm.Name())
			if err != nil {
				return err
			}

			if !isImport {
				// Remove the storage volume, snapshot volumes and database records.
				err = pool.DeleteInstance(vm, nil)
				if err != nil {
					return err
				}
			}
		}
	}

	// Perform other cleanup steps if not snapshot.
	if !vm.IsSnapshot() {
		// Remove all backups.
		backups, err := vm.Backups()
		if err != nil {
			return err
		}

		for _, backup := range backups {
			err = backup.Delete()
			if err != nil {
				return err
			}
		}

		// Delete the MAAS entry.
		err = vm.maasDelete()
		if err != nil {
			logger.Error("Failed deleting instance MAAS record", log.Ctx{"project": vm.Project(), "instance": vm.Name(), "err": err})
			return err
		}

		// Run device removal function for each device.
		for k, m := range vm.expandedDevices {
			err = vm.deviceRemove(k, m)
			if err != nil && err != device.ErrUnsupportedDevType {
				return errors.Wrapf(err, "Failed to remove device '%s'", k)
			}
		}

		// Clean things up.
		vm.cleanup()
	}

	// Remove the database record of the instance or snapshot instance.
	if err := vm.state.Cluster.InstanceRemove(vm.Project(), vm.Name()); err != nil {
		logger.Error("Failed deleting instance entry", log.Ctx{"project": vm.Project(), "instance": vm.Name(), "err": err})
		return err
	}

	logger.Info("Deleted instance", ctxMap)

	if vm.IsSnapshot() {
		vm.state.Events.SendLifecycle(vm.project, "virtual-machine-snapshot-deleted",
			fmt.Sprintf("/1.0/virtual-machines/%s", vm.name), map[string]interface{}{
				"snapshot_name": vm.name,
			})
	} else {
		vm.state.Events.SendLifecycle(vm.project, "virtual-machine-deleted",
			fmt.Sprintf("/1.0/virtual-machines/%s", vm.name), nil)
	}

	return nil
}

func (vm *qemu) deviceAdd(deviceName string, rawConfig deviceConfig.Device) error {
	d, _, err := vm.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return err
	}

	return d.Add()
}

func (vm *qemu) deviceRemove(deviceName string, rawConfig deviceConfig.Device) error {
	d, _, err := vm.deviceLoad(deviceName, rawConfig)

	// If deviceLoad fails with unsupported device type then return.
	if err == device.ErrUnsupportedDevType {
		return err
	}

	// If deviceLoad fails for any other reason then just log the error and proceed, as in the
	// scenario that a new version of LXD has additional validation restrictions than older
	// versions we still need to allow previously valid devices to be stopped.
	if err != nil {
		logger.Errorf("Device remove validation failed for '%s': %v", deviceName, err)
	}

	return d.Remove()
}

// Export publishes the instance.
func (vm *qemu) Export(w io.Writer, properties map[string]string) error {
	ctxMap := log.Ctx{
		"project":   vm.project,
		"name":      vm.name,
		"created":   vm.creationDate,
		"ephemeral": vm.ephemeral,
		"used":      vm.lastUsedDate}

	if vm.IsRunning() {
		return fmt.Errorf("Cannot export a running instance as an image")
	}

	logger.Info("Exporting instance", ctxMap)

	// Start the storage.
	ourStart, err := vm.mount()
	if err != nil {
		logger.Error("Failed exporting instance", ctxMap)
		return err
	}
	if ourStart {
		defer vm.unmount()
	}

	// Create the tarball.
	ctw := containerwriter.NewContainerTarWriter(w, nil)

	// Path inside the tar image is the pathname starting after cDir.
	cDir := vm.Path()
	offset := len(cDir) + 1

	writeToTar := func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		err = ctw.WriteFile(offset, path, fi)
		if err != nil {
			logger.Debugf("Error tarring up %s: %s", path, err)
			return err
		}

		return nil
	}

	// Look for metadata.yaml.
	fnam := filepath.Join(cDir, "metadata.yaml")
	if !shared.PathExists(fnam) {
		// Generate a new metadata.yaml.
		tempDir, err := ioutil.TempDir("", "lxd_lxd_metadata_")
		if err != nil {
			ctw.Close()
			logger.Error("Failed exporting instance", ctxMap)
			return err
		}
		defer os.RemoveAll(tempDir)

		// Get the instance's architecture.
		var arch string
		if vm.IsSnapshot() {
			parentName, _, _ := shared.InstanceGetParentAndSnapshotName(vm.name)
			parent, err := instance.LoadByProjectAndName(vm.state, vm.project, parentName)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting instance", ctxMap)
				return err
			}

			arch, _ = osarch.ArchitectureName(parent.Architecture())
		} else {
			arch, _ = osarch.ArchitectureName(vm.architecture)
		}

		if arch == "" {
			arch, err = osarch.ArchitectureName(vm.state.OS.Architectures[0])
			if err != nil {
				logger.Error("Failed exporting instance", ctxMap)
				return err
			}
		}

		// Fill in the metadata.
		meta := api.ImageMetadata{}
		meta.Architecture = arch
		meta.CreationDate = time.Now().UTC().Unix()
		meta.Properties = properties

		data, err := yaml.Marshal(&meta)
		if err != nil {
			ctw.Close()
			logger.Error("Failed exporting instance", ctxMap)
			return err
		}

		// Write the actual file.
		fnam = filepath.Join(tempDir, "metadata.yaml")
		err = ioutil.WriteFile(fnam, data, 0644)
		if err != nil {
			ctw.Close()
			logger.Error("Failed exporting instance", ctxMap)
			return err
		}

		fi, err := os.Lstat(fnam)
		if err != nil {
			ctw.Close()
			logger.Error("Failed exporting instance", ctxMap)
			return err
		}

		tmpOffset := len(filepath.Dir(fnam)) + 1
		if err := ctw.WriteFile(tmpOffset, fnam, fi); err != nil {
			ctw.Close()
			logger.Error("Failed exporting instance", ctxMap)
			return err
		}
	} else {
		if properties != nil {
			// Parse the metadata.
			content, err := ioutil.ReadFile(fnam)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting instance", ctxMap)
				return err
			}

			metadata := new(api.ImageMetadata)
			err = yaml.Unmarshal(content, &metadata)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting instance", ctxMap)
				return err
			}
			metadata.Properties = properties

			// Generate a new metadata.yaml.
			tempDir, err := ioutil.TempDir("", "lxd_lxd_metadata_")
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting instance", ctxMap)
				return err
			}
			defer os.RemoveAll(tempDir)

			data, err := yaml.Marshal(&metadata)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting instance", ctxMap)
				return err
			}

			// Write the actual file.
			fnam = filepath.Join(tempDir, "metadata.yaml")
			err = ioutil.WriteFile(fnam, data, 0644)
			if err != nil {
				ctw.Close()
				logger.Error("Failed exporting instance", ctxMap)
				return err
			}
		}

		// Include metadata.yaml in the tarball.
		fi, err := os.Lstat(fnam)
		if err != nil {
			ctw.Close()
			logger.Debugf("Error statting %s during export", fnam)
			logger.Error("Failed exporting instance", ctxMap)
			return err
		}

		if properties != nil {
			tmpOffset := len(filepath.Dir(fnam)) + 1
			err = ctw.WriteFile(tmpOffset, fnam, fi)
		} else {
			err = ctw.WriteFile(offset, fnam, fi)
		}
		if err != nil {
			ctw.Close()
			logger.Debugf("Error writing to tarfile: %s", err)
			logger.Error("Failed exporting instance", ctxMap)
			return err
		}
	}

	// Convert and include the root image.
	pool, err := vm.getStoragePool()
	if err != nil {
		return err
	}

	rootDrivePath, err := pool.GetInstanceDisk(vm)
	if err != nil {
		return err
	}

	// Convert from raw to qcow2 and add to tarball.
	tmpPath, err := ioutil.TempDir("", "lxd_export_")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpPath)

	fPath := fmt.Sprintf("%s/rootfs.img", tmpPath)
	_, err = shared.RunCommand("qemu-img", "convert", "-c", "-O", "qcow2", rootDrivePath, fPath)
	if err != nil {
		return fmt.Errorf("Failed converting image to qcow2: %v", err)
	}

	fi, err := os.Lstat(fPath)
	if err != nil {
		return err
	}

	err = ctw.WriteFile(len(tmpPath)+1, fPath, fi)
	if err != nil {
		return err
	}

	// Include all the templates.
	fnam = vm.TemplatesPath()
	if shared.PathExists(fnam) {
		err = filepath.Walk(fnam, writeToTar)
		if err != nil {
			logger.Error("Failed exporting instance", ctxMap)
			return err
		}
	}

	err = ctw.Close()
	if err != nil {
		logger.Error("Failed exporting instance", ctxMap)
		return err
	}

	logger.Info("Exported instance", ctxMap)
	return nil
}

// Migrate migrates the instance to another node.
func (vm *qemu) Migrate(args *instance.CriuMigrationArgs) error {
	return instance.ErrNotImplemented
}

// CGroupSet is not implemented for VMs.
func (vm *qemu) CGroupSet(key string, value string) error {
	return instance.ErrNotImplemented
}

// VolatileSet sets one or more volatile config keys.
func (vm *qemu) VolatileSet(changes map[string]string) error {
	// Sanity check.
	for key := range changes {
		if !strings.HasPrefix(key, "volatile.") {
			return fmt.Errorf("Only volatile keys can be modified with VolatileSet")
		}
	}

	// Update the database.
	var err error
	if vm.IsSnapshot() {
		err = vm.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
			return tx.InstanceSnapshotConfigUpdate(vm.id, changes)
		})
	} else {
		err = vm.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
			return tx.ContainerConfigUpdate(vm.id, changes)
		})
	}
	if err != nil {
		return errors.Wrap(err, "Failed to volatile config")
	}

	// Apply the change locally.
	for key, value := range changes {
		if value == "" {
			delete(vm.expandedConfig, key)
			delete(vm.localConfig, key)
			continue
		}

		vm.expandedConfig[key] = value
		vm.localConfig[key] = value
	}

	return nil
}

// FileExists is not implemented for VMs.
func (vm *qemu) FileExists(path string) error {
	return instance.ErrNotImplemented
}

// FilePull retrieves a file from the instance.
func (vm *qemu) FilePull(srcPath string, dstPath string) (int64, int64, os.FileMode, string, []string, error) {
	client, err := vm.getAgentClient()
	if err != nil {
		return 0, 0, 0, "", nil, err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		logger.Errorf("Failed to connect to lxd-agent on %s: %v", vm.Name(), err)
		return 0, 0, 0, "", nil, fmt.Errorf("Failed to connect to lxd-agent")
	}
	defer agent.Disconnect()

	content, resp, err := agent.GetInstanceFile("", srcPath)
	if err != nil {
		return 0, 0, 0, "", nil, err
	}

	switch resp.Type {
	case "file", "symlink":
		data, err := ioutil.ReadAll(content)
		if err != nil {
			return 0, 0, 0, "", nil, err
		}

		err = ioutil.WriteFile(dstPath, data, os.FileMode(resp.Mode))
		if err != nil {
			return 0, 0, 0, "", nil, err
		}

		err = os.Lchown(dstPath, int(resp.UID), int(resp.GID))
		if err != nil {
			return 0, 0, 0, "", nil, err
		}

		return resp.UID, resp.GID, os.FileMode(resp.Mode), resp.Type, nil, nil
	case "directory":
		return resp.UID, resp.GID, os.FileMode(resp.Mode), resp.Type, resp.Entries, nil
	}

	return 0, 0, 0, "", nil, fmt.Errorf("bad file type %s", resp.Type)
}

// FilePush pushes a file into the instance.
func (vm *qemu) FilePush(fileType string, srcPath string, dstPath string, uid int64, gid int64, mode int, write string) error {
	client, err := vm.getAgentClient()
	if err != nil {
		return err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		logger.Errorf("Failed to connect to lxd-agent on %s: %v", vm.Name(), err)
		return fmt.Errorf("Failed to connect to lxd-agent")
	}
	defer agent.Disconnect()

	args := lxdClient.InstanceFileArgs{
		GID:       gid,
		Mode:      mode,
		Type:      fileType,
		UID:       uid,
		WriteMode: write,
	}

	if fileType == "file" {
		f, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer f.Close()

		args.Content = f
	} else if fileType == "symlink" {
		symlinkTarget, err := os.Readlink(dstPath)
		if err != nil {
			return err
		}

		args.Content = bytes.NewReader([]byte(symlinkTarget))
	}

	err = agent.CreateInstanceFile("", dstPath, args)
	if err != nil {
		return err
	}

	return nil
}

// FileRemove removes a file from the instance.
func (vm *qemu) FileRemove(path string) error {
	// Connect to the agent.
	client, err := vm.getAgentClient()
	if err != nil {
		return err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		return fmt.Errorf("Failed to connect to lxd-agent")
	}
	defer agent.Disconnect()

	// Delete instance file.
	err = agent.DeleteInstanceFile("", path)
	if err != nil {
		return err
	}

	return nil
}

// Console gets access to the instance's console.
func (vm *qemu) Console() (*os.File, chan error, error) {
	chDisconnect := make(chan error, 1)

	// Avoid duplicate connects.
	vmConsoleLock.Lock()
	if vmConsole[vm.id] {
		vmConsoleLock.Unlock()
		return nil, nil, fmt.Errorf("There is already an active console for this instance")
	}
	vmConsoleLock.Unlock()

	// Connect to the monitor.
	monitor, err := qmp.Connect(vm.getMonitorPath(), vm.getMonitorEventHandler())
	if err != nil {
		return nil, nil, err // The VM isn't running as no monitor socket available.
	}

	// Get the console.
	console, err := monitor.Console("console")
	if err != nil {
		return nil, nil, err
	}

	// Record the console is in use.
	vmConsoleLock.Lock()
	vmConsole[vm.id] = true
	vmConsoleLock.Unlock()

	// Handle console disconnection.
	go func() {
		<-chDisconnect

		vmConsoleLock.Lock()
		delete(vmConsole, vm.id)
		vmConsoleLock.Unlock()
	}()

	return console, chDisconnect, nil
}

// Exec a command inside the instance.
func (vm *qemu) Exec(req api.InstanceExecPost, stdin *os.File, stdout *os.File, stderr *os.File) (instance.Cmd, error) {
	revert := revert.New()
	defer revert.Fail()

	client, err := vm.getAgentClient()
	if err != nil {
		return nil, err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		logger.Errorf("Failed to connect to lxd-agent on %s: %v", vm.Name(), err)
		return nil, fmt.Errorf("Failed to connect to lxd-agent")
	}
	revert.Add(agent.Disconnect)

	req.WaitForWS = true
	if req.Interactive {
		// Set console to raw.
		oldttystate, err := termios.MakeRaw(int(stdin.Fd()))
		if err != nil {
			return nil, err
		}

		revert.Add(func() { termios.Restore(int(stdin.Fd()), oldttystate) })
	}

	dataDone := make(chan bool)
	controlSendCh := make(chan api.InstanceExecControl)
	controlResCh := make(chan error)

	// This is the signal control handler, it receives signals from lxc CLI and forwards them to the VM agent.
	controlHandler := func(control *websocket.Conn) {
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		defer control.WriteMessage(websocket.CloseMessage, closeMsg)

		for {
			select {
			case cmd := <-controlSendCh:
				controlResCh <- control.WriteJSON(cmd)
			case <-dataDone:
				return
			}
		}
	}

	args := lxdClient.InstanceExecArgs{
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stderr,
		DataDone: dataDone,
		Control:  controlHandler,
	}

	op, err := agent.ExecInstance("", req, &args)
	if err != nil {
		return nil, err
	}

	instCmd := &qemuCmd{
		cmd:              op,
		attachedChildPid: 0, // Process is not running on LXD host.
		dataDone:         args.DataDone,
		cleanupFunc:      revert.Clone().Fail, // Pass revert function clone as clean up function.
		controlSendCh:    controlSendCh,
		controlResCh:     controlResCh,
	}

	revert.Success()
	return instCmd, nil
}

// Render returns info about the instance.
func (vm *qemu) Render() (interface{}, interface{}, error) {
	if vm.IsSnapshot() {
		// Prepare the ETag
		etag := []interface{}{vm.expiryDate}

		vmSnap := api.InstanceSnapshot{
			CreatedAt:       vm.creationDate,
			ExpandedConfig:  vm.expandedConfig,
			ExpandedDevices: vm.expandedDevices.CloneNative(),
			LastUsedAt:      vm.lastUsedDate,
			Name:            strings.SplitN(vm.name, "/", 2)[1],
			Stateful:        vm.stateful,
		}
		vmSnap.Architecture = vm.architectureName
		vmSnap.Config = vm.localConfig
		vmSnap.Devices = vm.localDevices.CloneNative()
		vmSnap.Ephemeral = vm.ephemeral
		vmSnap.Profiles = vm.profiles
		vmSnap.ExpiresAt = vm.expiryDate

		return &vmSnap, etag, nil
	}

	// Prepare the ETag
	etag := []interface{}{vm.architecture, vm.localConfig, vm.localDevices, vm.ephemeral, vm.profiles}

	vmState := api.Instance{
		ExpandedConfig:  vm.expandedConfig,
		ExpandedDevices: vm.expandedDevices.CloneNative(),
		Name:            vm.name,
		Status:          vm.statusCode().String(),
		StatusCode:      vm.statusCode(),
		Location:        vm.node,
		Type:            vm.Type().String(),
	}

	vmState.Description = vm.description
	vmState.Architecture = vm.architectureName
	vmState.Config = vm.localConfig
	vmState.CreatedAt = vm.creationDate
	vmState.Devices = vm.localDevices.CloneNative()
	vmState.Ephemeral = vm.ephemeral
	vmState.LastUsedAt = vm.lastUsedDate
	vmState.Profiles = vm.profiles
	vmState.Stateful = vm.stateful

	return &vmState, etag, nil
}

// RenderFull returns all info about the instance.
func (vm *qemu) RenderFull() (*api.InstanceFull, interface{}, error) {
	if vm.IsSnapshot() {
		return nil, nil, fmt.Errorf("RenderFull doesn't work with snapshots")
	}

	// Get the Instance struct.
	base, etag, err := vm.Render()
	if err != nil {
		return nil, nil, err
	}

	// Convert to InstanceFull.
	vmState := api.InstanceFull{Instance: *base.(*api.Instance)}

	// Add the InstanceState.
	vmState.State, err = vm.RenderState()
	if err != nil {
		return nil, nil, err
	}

	// Add the InstanceSnapshots.
	snaps, err := vm.Snapshots()
	if err != nil {
		return nil, nil, err
	}

	for _, snap := range snaps {
		render, _, err := snap.Render()
		if err != nil {
			return nil, nil, err
		}

		if vmState.Snapshots == nil {
			vmState.Snapshots = []api.InstanceSnapshot{}
		}

		vmState.Snapshots = append(vmState.Snapshots, *render.(*api.InstanceSnapshot))
	}

	// Add the InstanceBackups.
	backups, err := vm.Backups()
	if err != nil {
		return nil, nil, err
	}

	for _, backup := range backups {
		render := backup.Render()

		if vmState.Backups == nil {
			vmState.Backups = []api.InstanceBackup{}
		}

		vmState.Backups = append(vmState.Backups, *render)
	}

	return &vmState, etag, nil
}

// RenderState returns just state info about the instance.
func (vm *qemu) RenderState() (*api.InstanceState, error) {
	statusCode := vm.statusCode()
	pid, _ := vm.pid()

	if statusCode == api.Running {
		status, err := vm.agentGetState()
		if err != nil {
			if err != errQemuAgentOffline {
				logger.Warn("Could not get VM state from agent", log.Ctx{"project": vm.Project(), "instance": vm.Name(), "err": err})
			}

			// Fallback data.
			status = &api.InstanceState{}
			status.Processes = -1
			networks := map[string]api.InstanceStateNetwork{}
			for k, m := range vm.ExpandedDevices() {
				// We only care about nics.
				if m["type"] != "nic" || m.NICType() != "bridged" {
					continue
				}

				// Fill the MAC address.
				m, err := vm.FillNetworkDevice(k, m)
				if err != nil {
					return nil, err
				}

				// Parse the lease file.
				addresses, err := network.GetLeaseAddresses(vm.state, m["parent"], m["hwaddr"])
				if err != nil {
					return nil, err
				}

				if len(addresses) == 0 {
					continue
				}

				// Get MTU.
				iface, err := net.InterfaceByName(m["parent"])
				if err != nil {
					return nil, err
				}

				if m["host_name"] == "" {
					m["host_name"] = vm.localConfig[fmt.Sprintf("volatile.%s.host_name", k)]
				}

				// Retrieve the host counters, as we report the values
				// from the instance's point of view, those counters need to be reversed below.
				hostCounters := shared.NetworkGetCounters(m["host_name"])

				networks[k] = api.InstanceStateNetwork{
					Addresses: addresses,
					Counters: api.InstanceStateNetworkCounters{
						BytesReceived:   hostCounters.BytesSent,
						BytesSent:       hostCounters.BytesReceived,
						PacketsReceived: hostCounters.PacketsSent,
						PacketsSent:     hostCounters.PacketsReceived,
					},
					Hwaddr:   m["hwaddr"],
					HostName: m["host_name"],
					Mtu:      iface.MTU,
					State:    "up",
					Type:     "broadcast",
				}
			}

			status.Network = networks
		}

		status.Pid = int64(pid)
		status.Status = statusCode.String()
		status.StatusCode = statusCode
		status.Disk, err = vm.diskState()
		if err != nil && err != storageDrivers.ErrNotSupported {
			logger.Warn("Error getting disk usage", log.Ctx{"project": vm.Project(), "instance": vm.Name(), "err": err})
		}

		return status, nil
	}

	// At least return the Status and StatusCode if we couldn't get any
	// information for the VM agent.
	return &api.InstanceState{
		Pid:        int64(pid),
		Status:     statusCode.String(),
		StatusCode: statusCode,
	}, nil
}

// diskState gets disk usage info.
func (vm *qemu) diskState() (map[string]api.InstanceStateDisk, error) {
	pool, err := vm.getStoragePool()
	if err != nil {
		return nil, err
	}

	// Get the root disk device config.
	rootDiskName, _, err := shared.GetRootDiskDevice(vm.ExpandedDevices().CloneNative())
	if err != nil {
		return nil, err
	}

	usage, err := pool.GetInstanceUsage(vm)
	if err != nil {
		return nil, err
	}

	disk := map[string]api.InstanceStateDisk{}
	disk[rootDiskName] = api.InstanceStateDisk{Usage: usage}
	return disk, nil
}

// agentGetState connects to the agent inside of the VM and does
// an API call to get the current state.
func (vm *qemu) agentGetState() (*api.InstanceState, error) {
	// Check if the agent is running.
	monitor, err := qmp.Connect(vm.getMonitorPath(), vm.getMonitorEventHandler())
	if err != nil {
		return nil, err
	}

	if !monitor.AgentReady() {
		return nil, errQemuAgentOffline
	}

	client, err := vm.getAgentClient()
	if err != nil {
		return nil, err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		return nil, err
	}
	defer agent.Disconnect()

	status, _, err := agent.GetInstanceState("")
	if err != nil {
		return nil, err
	}

	return status, nil
}

// IsRunning returns whether or not the instance is running.
func (vm *qemu) IsRunning() bool {
	state := vm.State()
	return state != "BROKEN" && state != "STOPPED"
}

// IsFrozen returns whether the instance frozen or not.
func (vm *qemu) IsFrozen() bool {
	return vm.State() == "FROZEN"
}

// IsEphemeral returns whether the instanc is ephemeral or not.
func (vm *qemu) IsEphemeral() bool {
	return vm.ephemeral
}

// IsSnapshot returns whether instance is snapshot or not.
func (vm *qemu) IsSnapshot() bool {
	return vm.snapshot
}

// IsStateful retuens whether instance is stateful or not.
func (vm *qemu) IsStateful() bool {
	return vm.stateful
}

// DeviceEventHandler handles events occurring on the instance's devices.
func (vm *qemu) DeviceEventHandler(runConf *deviceConfig.RunConfig) error {
	return fmt.Errorf("DeviceEventHandler Not implemented")
}

// ID returns the instance's ID.
func (vm *qemu) ID() int {
	return vm.id
}

// vsockID returns the vsock context ID, 3 being the first ID that can be used.
func (vm *qemu) vsockID() int {
	return vm.id + 3
}

// Location returns instance's location.
func (vm *qemu) Location() string {
	return vm.node
}

// Project returns instance's project.
func (vm *qemu) Project() string {
	return vm.project
}

// Name returns the instance's name.
func (vm *qemu) Name() string {
	return vm.name
}

// Description returns the instance's description.
func (vm *qemu) Description() string {
	return vm.description
}

// Architecture returns the instance's architecture.
func (vm *qemu) Architecture() int {
	return vm.architecture
}

// CreationDate returns the instance's creation date.
func (vm *qemu) CreationDate() time.Time {
	return vm.creationDate
}

// LastUsedDate returns the instance's last used date.
func (vm *qemu) LastUsedDate() time.Time {
	return vm.lastUsedDate
}

// Profiles returns the instance's profiles.
func (vm *qemu) Profiles() []string {
	return vm.profiles
}

// InitPID returns the instance's current process ID.
func (vm *qemu) InitPID() int {
	pid, _ := vm.pid()
	return pid
}

func (vm *qemu) statusCode() api.StatusCode {
	// Connect to the monitor.
	monitor, err := qmp.Connect(vm.getMonitorPath(), vm.getMonitorEventHandler())
	if err != nil {
		// If we fail to connect, chances are the VM isn't running.
		return api.Stopped
	}

	status, err := monitor.Status()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			return api.Stopped
		}

		return api.Error
	}

	if status == "running" {
		return api.Running
	} else if status == "paused" {
		return api.Frozen
	}

	return api.Stopped
}

// State returns the instance's state code.
func (vm *qemu) State() string {
	return strings.ToUpper(vm.statusCode().String())
}

// ExpiryDate returns when this snapshot expires.
func (vm *qemu) ExpiryDate() time.Time {
	if vm.IsSnapshot() {
		return vm.expiryDate
	}

	// Return zero time if the instance is not a snapshot.
	return time.Time{}
}

// Path returns the instance's path.
func (vm *qemu) Path() string {
	return storagePools.InstancePath(vm.Type(), vm.Project(), vm.Name(), vm.IsSnapshot())
}

// DevicesPath returns the instance's devices path.
func (vm *qemu) DevicesPath() string {
	name := project.Instance(vm.Project(), vm.Name())
	return shared.VarPath("devices", name)
}

// ShmountsPath returns the instance's shared mounts path.
func (vm *qemu) ShmountsPath() string {
	name := project.Instance(vm.Project(), vm.Name())
	return shared.VarPath("shmounts", name)
}

// LogPath returns the instance's log path.
func (vm *qemu) LogPath() string {
	name := project.Instance(vm.Project(), vm.Name())
	return shared.LogPath(name)
}

// LogFilePath returns the instance's log path.
func (vm *qemu) LogFilePath() string {
	return filepath.Join(vm.LogPath(), "qemu.log")
}

// ConsoleBufferLogPath returns the instance's console buffer log path.
func (vm *qemu) ConsoleBufferLogPath() string {
	return filepath.Join(vm.LogPath(), "console.log")
}

// RootfsPath returns the instance's rootfs path.
func (vm *qemu) RootfsPath() string {
	return filepath.Join(vm.Path(), "rootfs")
}

// TemplatesPath returns the instance's templates path.
func (vm *qemu) TemplatesPath() string {
	return filepath.Join(vm.Path(), "templates")
}

// StatePath returns the instance's state path.
func (vm *qemu) StatePath() string {
	return filepath.Join(vm.Path(), "state")
}

// StoragePool returns the name of the instance's storage pool.
func (vm *qemu) StoragePool() (string, error) {
	poolName, err := vm.state.Cluster.InstancePool(vm.Project(), vm.Name())
	if err != nil {
		return "", err
	}

	return poolName, nil
}

// SetOperation sets the current operation.
func (vm *qemu) SetOperation(op *operations.Operation) {
	vm.op = op
}

// StorageStart deprecated.
func (vm *qemu) StorageStart() (bool, error) {
	return false, storagePools.ErrNotImplemented
}

// StorageStop deprecated.
func (vm *qemu) StorageStop() (bool, error) {
	return false, storagePools.ErrNotImplemented
}

// DeferTemplateApply not used currently.
func (vm *qemu) DeferTemplateApply(trigger string) error {
	err := vm.VolatileSet(map[string]string{"volatile.apply_template": trigger})
	if err != nil {
		return errors.Wrap(err, "Failed to set apply_template volatile key")
	}

	return nil
}

// FillNetworkDevice takes a nic or infiniband device type and enriches it with automatically
// generated name and hwaddr properties if these are missing from the device.
func (vm *qemu) FillNetworkDevice(name string, m deviceConfig.Device) (deviceConfig.Device, error) {
	var err error

	newDevice := m.Clone()
	updateKey := func(key string, value string) error {
		tx, err := vm.state.Cluster.Begin()
		if err != nil {
			return err
		}

		err = db.ContainerConfigInsert(tx, vm.id, map[string]string{key: value})
		if err != nil {
			tx.Rollback()
			return err
		}

		err = db.TxCommit(tx)
		if err != nil {
			return err
		}

		return nil
	}

	// Fill in the MAC address
	if !shared.StringInSlice(m.NICType(), []string{"physical", "ipvlan", "sriov"}) && m["hwaddr"] == "" {
		configKey := fmt.Sprintf("volatile.%s.hwaddr", name)
		volatileHwaddr := vm.localConfig[configKey]
		if volatileHwaddr == "" {
			// Generate a new MAC address
			volatileHwaddr, err = instance.DeviceNextInterfaceHWAddr()
			if err != nil {
				return nil, err
			}

			// Update the database
			err = query.Retry(func() error {
				err := updateKey(configKey, volatileHwaddr)
				if err != nil {
					// Check if something else filled it in behind our back
					value, err1 := vm.state.Cluster.ContainerConfigGet(vm.id, configKey)
					if err1 != nil || value == "" {
						return err
					}

					vm.localConfig[configKey] = value
					vm.expandedConfig[configKey] = value
					return nil
				}

				vm.localConfig[configKey] = volatileHwaddr
				vm.expandedConfig[configKey] = volatileHwaddr
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		newDevice["hwaddr"] = volatileHwaddr
	}

	return newDevice, nil
}

// Internal MAAS handling.
func (vm *qemu) maasInterfaces(devices map[string]map[string]string) ([]maas.ContainerInterface, error) {
	interfaces := []maas.ContainerInterface{}
	for k, m := range devices {
		if m["type"] != "nic" {
			continue
		}

		if m["maas.subnet.ipv4"] == "" && m["maas.subnet.ipv6"] == "" {
			continue
		}

		m, err := vm.FillNetworkDevice(k, m)
		if err != nil {
			return nil, err
		}

		subnets := []maas.ContainerInterfaceSubnet{}

		// IPv4
		if m["maas.subnet.ipv4"] != "" {
			subnet := maas.ContainerInterfaceSubnet{
				Name:    m["maas.subnet.ipv4"],
				Address: m["ipv4.address"],
			}

			subnets = append(subnets, subnet)
		}

		// IPv6
		if m["maas.subnet.ipv6"] != "" {
			subnet := maas.ContainerInterfaceSubnet{
				Name:    m["maas.subnet.ipv6"],
				Address: m["ipv6.address"],
			}

			subnets = append(subnets, subnet)
		}

		iface := maas.ContainerInterface{
			Name:       m["name"],
			MACAddress: m["hwaddr"],
			Subnets:    subnets,
		}

		interfaces = append(interfaces, iface)
	}

	return interfaces, nil
}

func (vm *qemu) maasRename(newName string) error {
	maasURL, err := cluster.ConfigGetString(vm.state.Cluster, "maas.api.url")
	if err != nil {
		return err
	}

	if maasURL == "" {
		return nil
	}

	interfaces, err := vm.maasInterfaces(vm.expandedDevices.CloneNative())
	if err != nil {
		return err
	}

	if len(interfaces) == 0 {
		return nil
	}

	if vm.state.MAAS == nil {
		return fmt.Errorf("Can't perform the operation because MAAS is currently unavailable")
	}

	exists, err := vm.state.MAAS.DefinedContainer(project.Instance(vm.project, vm.name))
	if err != nil {
		return err
	}

	if !exists {
		return vm.maasUpdate(nil)
	}

	return vm.state.MAAS.RenameContainer(project.Instance(vm.project, vm.name), project.Instance(vm.project, newName))
}

func (vm *qemu) maasDelete() error {
	maasURL, err := cluster.ConfigGetString(vm.state.Cluster, "maas.api.url")
	if err != nil {
		return err
	}

	if maasURL == "" {
		return nil
	}

	interfaces, err := vm.maasInterfaces(vm.expandedDevices.CloneNative())
	if err != nil {
		return err
	}

	if len(interfaces) == 0 {
		return nil
	}

	if vm.state.MAAS == nil {
		return fmt.Errorf("Can't perform the operation because MAAS is currently unavailable")
	}

	exists, err := vm.state.MAAS.DefinedContainer(project.Instance(vm.project, vm.name))
	if err != nil {
		return err
	}

	if !exists {
		return nil
	}

	return vm.state.MAAS.DeleteContainer(project.Instance(vm.project, vm.name))
}

func (vm *qemu) maasUpdate(oldDevices map[string]map[string]string) error {
	// Check if MAAS is configured
	maasURL, err := cluster.ConfigGetString(vm.state.Cluster, "maas.api.url")
	if err != nil {
		return err
	}

	if maasURL == "" {
		return nil
	}

	// Check if there's something that uses MAAS
	interfaces, err := vm.maasInterfaces(vm.expandedDevices.CloneNative())
	if err != nil {
		return err
	}

	var oldInterfaces []maas.ContainerInterface
	if oldDevices != nil {
		oldInterfaces, err = vm.maasInterfaces(oldDevices)
		if err != nil {
			return err
		}
	}

	if len(interfaces) == 0 && len(oldInterfaces) == 0 {
		return nil
	}

	// See if we're connected to MAAS
	if vm.state.MAAS == nil {
		return fmt.Errorf("Can't perform the operation because MAAS is currently unavailable")
	}

	exists, err := vm.state.MAAS.DefinedContainer(project.Instance(vm.project, vm.name))
	if err != nil {
		return err
	}

	if exists {
		if len(interfaces) == 0 && len(oldInterfaces) > 0 {
			return vm.state.MAAS.DeleteContainer(project.Instance(vm.project, vm.name))
		}

		return vm.state.MAAS.UpdateContainer(project.Instance(vm.project, vm.name), interfaces)
	}

	return vm.state.MAAS.CreateContainer(project.Instance(vm.project, vm.name), interfaces)
}

// UpdateBackupFile writes the instance's backup.yaml file to storage.
func (vm *qemu) UpdateBackupFile() error {
	pool, err := vm.getStoragePool()
	if err != nil {
		return err
	}

	return pool.UpdateInstanceBackupFile(vm, nil)
}

func (vm *qemu) cpuTopology(limit string) (int, int, int, map[uint64]uint64, error) {
	// Get CPU topology.
	cpus, err := resources.GetCPU()
	if err != nil {
		return -1, -1, -1, nil, err
	}

	// Expand the pins.
	pins, err := instance.ParseCpuset(limit)
	if err != nil {
		return -1, -1, -1, nil, err
	}

	// Match tracking.
	vcpus := map[uint64]uint64{}
	sockets := map[uint64][]uint64{}
	cores := map[uint64][]uint64{}

	// Go through the physical CPUs looking for matches.
	i := uint64(0)
	for _, cpu := range cpus.Sockets {
		for _, core := range cpu.Cores {
			for _, thread := range core.Threads {
				for _, pin := range pins {
					if thread.ID == int64(pin) {
						// Found a matching CPU.
						vcpus[i] = uint64(pin)
						i++

						// Track cores per socket.
						_, ok := sockets[cpu.Socket]
						if !ok {
							sockets[cpu.Socket] = []uint64{}
						}
						if !shared.Uint64InSlice(core.Core, sockets[cpu.Socket]) {
							sockets[cpu.Socket] = append(sockets[cpu.Socket], core.Core)
						}

						// Track threads per core.
						_, ok = cores[core.Core]
						if !ok {
							cores[core.Core] = []uint64{}
						}
						if !shared.Uint64InSlice(thread.Thread, cores[core.Core]) {
							cores[core.Core] = append(cores[core.Core], thread.Thread)
						}
					}
				}
			}
		}
	}

	// Confirm we're getting the expected number of CPUs.
	if len(pins) != len(vcpus) {
		return -1, -1, -1, nil, fmt.Errorf("Unavailable CPUs requested: %s", limit)
	}

	// Validate the topology.
	valid := true
	nrSockets := 0
	nrCores := 0
	nrThreads := 0

	// Confirm that there is no balancing inconsistencies.
	countCores := -1
	for _, cores := range sockets {
		if countCores != -1 && len(cores) != countCores {
			valid = false
			break
		}

		countCores = len(cores)
	}

	countThreads := -1
	for _, threads := range cores {
		if countThreads != -1 && len(threads) != countThreads {
			valid = false
			break
		}

		countThreads = len(threads)
	}

	// Check against double listing of CPU.
	if len(sockets)*countCores*countThreads != len(vcpus) {
		valid = false
	}

	// Build up the topology.
	if valid {
		// Valid topology.
		nrSockets = len(sockets)
		nrCores = countCores
		nrThreads = countThreads
	} else {
		logger.Warnf("Instance '%s' uses a CPU pinning profile which doesn't match hardware layout", project.Instance(vm.Project(), vm.Name()))

		// Fallback on pretending everything are cores.
		nrSockets = 1
		nrCores = len(vcpus)
		nrThreads = 1
	}

	return nrSockets, nrCores, nrThreads, vcpus, nil
}
