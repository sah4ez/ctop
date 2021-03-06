package connector

import (
	"fmt"

	"github.com/bcicen/ctop/entity"
	"github.com/bcicen/ctop/logging"
	"github.com/bcicen/ctop/models"
)

var log = logging.Init()

// ByName return connector via Name from map
func ByName(s string) (Connector, error) {
	if _, ok := enabled[s]; !ok {
		msg := fmt.Sprintf("invalid connector type \"%s\"\nconnector must be one of:", s)
		for k := range enabled {
			msg += fmt.Sprintf("\n  %s", k)
		}
		return nil, fmt.Errorf(msg)
	}
	return enabled[s](), nil
}

// Connector it is interface for implentation different types of connector (Docker, Runc, Kubernets and etc.)
type Connector interface {
	AllNodes() entity.Nodes
	AllServices() entity.Services
	AllContainers() entity.Containers
	AllTasks() entity.Tasks
	GetContainer(string) (*entity.Container, bool)
	GetService(string) (*entity.Service, bool)
	GetTask(string) (*entity.Task, bool)
	Down()
	SetMetrics(metrics models.Metrics)
}
