// +build !darwin

package connector

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/bcicen/ctop/connector/collector"
	"github.com/bcicen/ctop/connector/manager"
	"github.com/bcicen/ctop/entity"
	"github.com/bcicen/ctop/models"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/cgroups/systemd"
)

type RuncOpts struct {
	root           string // runc root path
	systemdCgroups bool   // use systemd cgroups
}

func NewRuncOpts() (RuncOpts, error) {
	var opts RuncOpts
	// read runc root path
	root := os.Getenv("RUNC_ROOT")
	if root == "" {
		root = "/run/runc"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return opts, err
	}
	opts.root = abs

	// ensure runc root path is readable
	_, err = ioutil.ReadDir(opts.root)
	if err != nil {
		return opts, err
	}

	if os.Getenv("RUNC_SYSTEMD_CGROUP") == "1" {
		opts.systemdCgroups = true
	}
	return opts, nil
}

type Runc struct {
	opts          RuncOpts
	factory       libcontainer.Factory
	containers    map[string]*entity.Container
	libContainers map[string]libcontainer.Container
	needsRefresh  chan string // container IDs requiring refresh
	lock          sync.RWMutex
}

func NewRunc() Connector {
	opts, err := NewRuncOpts()
	runcFailOnErr(err)

	factory, err := getFactory(opts)
	runcFailOnErr(err)

	cm := &Runc{
		opts:          opts,
		factory:       factory,
		containers:    make(map[string]*entity.Container),
		libContainers: make(map[string]libcontainer.Container),
		needsRefresh:  make(chan string, 60),
		lock:          sync.RWMutex{},
	}
	go cm.Loop()

	return cm
}

func (cm *Runc) GetLibc(id string) libcontainer.Container {
	// return previously loaded container
	libc, ok := cm.libContainers[id]
	if ok {
		return libc
	}
	// load container
	libc, err := cm.factory.Load(id)
	if err != nil {
		// remove container if no longer exists
		if lerr, ok := err.(libcontainer.Error); ok && lerr.Code() == libcontainer.ContainerNotExists {
			cm.delByID(id)
		} else {
			log.Warningf("failed to read container: %s\n", err)
		}
		return nil
	}
	return libc
}

// update a ctop container from libcontainer
func (cm *Runc) refresh(id string) {
	libc := cm.GetLibc(id)
	if libc == nil {
		return
	}
	c := cm.MustGet(id)

	// remove container if entered destroyed state on last refresh
	// this gives adequate time for the collector to be shut down
	if c.GetMeta("state") == "destroyed" {
		cm.delByID(id)
		return
	}

	status, err := libc.Status()
	if err != nil {
		log.Warningf("failed to read status for container: %s\n", err)
	} else {
		c.SetState(status.String())
	}

	state, err := libc.State()
	if err != nil {
		log.Warningf("failed to read state for container: %s\n", err)
	} else {
		c.SetMeta("created", state.BaseState.Created.Format("Mon Jan 2 15:04:05 2006"))
	}

	conf := libc.Config()
	c.SetMeta("rootfs", conf.Rootfs)
}

// Read runc root, creating any new containers
func (cm *Runc) refreshAllContainers() {
	list, err := ioutil.ReadDir(cm.opts.root)
	runcFailOnErr(err)

	for _, i := range list {
		if i.IsDir() {
			name := i.Name()
			// attempt to load
			libc := cm.GetLibc(name)
			if libc == nil {
				continue
			}
			_ = cm.MustGet(i.Name()) // ensure container exists
		}
	}

	// queue all existing containers for refresh
	for id := range cm.containers {
		cm.needsRefresh <- id
	}
	log.Debugf("queued %d containers for refresh", len(cm.containers))
}

func (cm *Runc) Loop() {
	for id := range cm.needsRefresh {
		cm.refresh(id)
	}
}

// Get a single ctop container in the map matching libc container, creating one anew if not existing
func (cm *Runc) MustGet(id string) *entity.Container {
	c, ok := cm.GetContainer(id)
	if !ok {
		libc := cm.GetLibc(id)

		// create collector
		collector := collector.NewRunc(libc)

		// create container
		manager := manager.NewRunc()
		c = entity.NewContainer(id, collector, manager)

		name := libc.ID()
		// set initial metadata
		if len(name) > 12 {
			name = name[0:12]
		}
		c.SetMeta("name", name)

		// add to map
		cm.lock.Lock()
		cm.containers[id] = c
		cm.libContainers[id] = libc
		cm.lock.Unlock()
		log.Debugf("saw new container: %s", id)
	}

	return c
}

// Get a single container, by ID
func (cm *Runc) GetContainer(id string) (*entity.Container, bool) {
	cm.lock.Lock()
	defer cm.lock.Unlock()
	c, ok := cm.containers[id]
	return c, ok
}

func (cm *Runc) GetTask(id string) (*entity.Task, bool) {
	return nil, false
}

func (cm *Runc) GetService(id string) (s *entity.Service, ok bool) {
	return s, ok
}

// Remove containers by ID
func (cm *Runc) delByID(id string) {
	cm.lock.Lock()
	delete(cm.containers, id)
	delete(cm.libContainers, id)
	cm.lock.Unlock()
	log.Infof("removed dead container: %s", id)
}

func (cm *Runc) AllNodes() (nodes entity.Nodes) {
	return nodes
}

func (cm *Runc) AllTasks() (tasks entity.Tasks) {
	return tasks
}

func (cm *Runc) AllServices() (services entity.Services) {
	return services
}

func (cm *Runc) AllContainers() (containers entity.Containers) {
	cm.lock.Lock()
	for _, container := range cm.containers {
		containers = append(containers, container)
	}
	containers.Filter()
	cm.lock.Unlock()
	return containers
}

func getFactory(opts RuncOpts) (libcontainer.Factory, error) {
	cgroupManager := libcontainer.Cgroupfs
	if opts.systemdCgroups {
		if systemd.UseSystemd() {
			cgroupManager = libcontainer.SystemdCgroups
		} else {
			return nil, fmt.Errorf("systemd cgroup enabled, but systemd support for managing cgroups is not available")
		}
	}
	return libcontainer.New(opts.root, cgroupManager)
}

func runcFailOnErr(err error) {
	if err != nil {
		panic(fmt.Errorf("fatal runc error: %s", err))
	}
}

func (cm *Runc) Down() {
	log.Warningf("Call unsupported method, Down()")
}

func (cm *Runc) SetMetrics(metrics models.Metrics) {
	if cont, ok := cm.GetContainer(metrics.Id); ok {
		cont.SetMetrics(metrics)
	}
}
