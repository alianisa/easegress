package informer

import (
	"fmt"
	"sync"

	yamljsontool "github.com/ghodss/yaml"
	"github.com/tidwall/gjson"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/mvcc/mvccpb"
	"gopkg.in/yaml.v2"

	"github.com/megaease/easegateway/pkg/logger"
	"github.com/megaease/easegateway/pkg/object/meshcontroller/layout"
	"github.com/megaease/easegateway/pkg/object/meshcontroller/spec"
	"github.com/megaease/easegateway/pkg/object/meshcontroller/storage"
)

const (
	// TODO: Support EventCreate.

	// EventUpdate is the update inform event.
	EventUpdate = "Update"
	// EventDelete is the delete inform event.
	EventDelete = "Delete"

	// AllParts is the path of the whole structure.
	AllParts GJSONPath = ""

	// ServiceObservability  is the path of service observability.
	ServiceObservability GJSONPath = "observability"

	// ServiceResilience is the path of service resilience.
	ServiceResilience GJSONPath = "resilience"

	// ServiceCanary is the path of service canary.
	ServiceCanary GJSONPath = "canary"

	// ServiceLoadBalance is the path of service loadbalance.
	ServiceLoadBalance GJSONPath = "loadBalance"

	// ServiceCircuitBreaker is the path of service resilience's circuritBreaker part.
	ServiceCircuitBreaker GJSONPath = "resilience.circuitBreaker"
)

type (
	// Event is the type of inform event.
	Event struct {
		EventType string
		RawKV     *mvccpb.KeyValue
	}

	// GJSONPath is the type of inform path, in GJSON syntax.
	GJSONPath string

	specHandleFunc  func(event Event, value string) bool
	specsHandleFunc func(map[string]string) bool

	// The returning boolean flag of all callback functions means
	// if the stuff continues to be watched.

	// ServiceSpecFunc is the callback function type for service spec.
	ServiceSpecFunc func(event Event, serviceSpec *spec.Service) bool

	// ServiceSpecFunc is the callback function type for service specs.
	ServiceSpecsFunc func(value map[string]*spec.Service) bool

	// ServiceInstanceSpecFunc is the callback function type for service instance spec.
	ServicesInstanceSpecFunc func(event Event, instanceSpec *spec.ServiceInstanceSpec) bool

	// ServiceInstanceSpecFunc is the callback function type for service instance specs.
	ServiceInstanceSpecsFunc func(value map[string]*spec.ServiceInstanceSpec) bool

	// ServiceInstanceStatusFunc is the callback function type for service instance status.
	ServiceInstanceStatusFunc func(event Event, value *spec.ServiceInstanceStatus) bool

	// ServiceInstanceStatusesFunc is the callback function type for service instance statuses.
	ServiceInstanceStatusesFunc func(value map[string]*spec.ServiceInstanceStatus) bool

	// TenantSpecFunc is the callback function type for tenant spec.
	TenantSpecFunc func(event Event, value *spec.Tenant) bool

	// TenantSpecsFunc is the callback function type for tenant specs.
	TenantSpecsFunc func(value map[string]*spec.Tenant) bool

	// IngressSpecFunc is the callback function type for service spec.
	IngressSpecFunc func(event Event, ingressSpec *spec.Ingress) bool

	// IngressSpecFunc is the callback function type for service specs.
	IngressSpecsFunc func(value map[string]*spec.Ingress) bool

	// Informer is the interface for informing two type of storage changed for every Mesh spec structure.
	//  1. Based on comparison between old and new part of entry.
	//  2. Based on comparison on entries with the same prefix.
	Informer interface {
		OnPartOfServiceSpec(serviceName string, gjsonPath GJSONPath, fn ServiceSpecFunc) error
		OnServiceSpecs(servicePrefix string, fn ServiceSpecsFunc) error

		OnPartOfInstanceSpec(serviceName, instanceID string, gjsonPath GJSONPath, fn ServicesInstanceSpecFunc) error
		OnServiceInstanceSpecs(serviceName string, fn ServiceInstanceSpecsFunc) error

		OnPartOfServiceInstanceStatus(serviceName, instanceID string, gjsonPath GJSONPath, fn ServiceInstanceStatusFunc) error
		OnServiceInstanceStatuses(serviceName string, fn ServiceInstanceStatusesFunc) error

		OnPartOfTenantSpec(tenantName string, gjsonPath GJSONPath, fn TenantSpecFunc) error
		OnTenantSpecs(tenantPrefix string, fn TenantSpecsFunc) error

		OnPartOfIngressSpec(serviceName string, gjsonPath GJSONPath, fn IngressSpecFunc) error
		OnIngressSpecs(fn IngressSpecsFunc) error

		StopWatchServiceSpec(serviceName string, gjsonPath GJSONPath)
		StopWatchServiceInstanceSpec(serviceName string)

		Close()
	}

	// meshInformer is the informer for mesh usage
	meshInformer struct {
		mutex    sync.Mutex
		store    storage.Storage
		watchers map[string]storage.Watcher

		closed bool
		done   chan struct{}
	}
)

var (
	// ErrAlreadyWatched is the error when watch the same entry again.
	ErrAlreadyWatched = fmt.Errorf("already watched")

	// ErrClosed is the error when watching a closed informer.
	ErrClosed = fmt.Errorf("informer already been closed")

	// ErrNotFound is the error when watching an entry which is not found.
	ErrNotFound = fmt.Errorf("not found")
)

// NewInformer creates an informer.
func NewInformer(store storage.Storage) Informer {
	inf := &meshInformer{
		store:    store,
		watchers: make(map[string]storage.Watcher),
		mutex:    sync.Mutex{},
		done:     make(chan struct{}),
	}

	return inf
}

func (inf *meshInformer) stopWatchOneKey(key string) {
	inf.mutex.Lock()
	defer inf.mutex.Unlock()

	if watcher, exists := inf.watchers[key]; exists {
		watcher.Close()
		delete(inf.watchers, key)
	}
}

func serviceSpecWatcherKey(serviceName string, gjsonPath GJSONPath) string {
	return fmt.Sprintf("service-spec-%s-%s", serviceName, gjsonPath)
}

// OnPartOfServiceSpec watches one service's spec by given gjsonPath.
func (inf *meshInformer) OnPartOfServiceSpec(serviceName string, gjsonPath GJSONPath, fn ServiceSpecFunc) error {
	storeKey := layout.ServiceSpecKey(serviceName)
	watcherKey := serviceSpecWatcherKey(serviceName, gjsonPath)

	specFunc := func(event Event, value string) bool {
		serviceSpec := &spec.Service{}
		if event.EventType != EventDelete {
			if err := yaml.Unmarshal([]byte(value), serviceSpec); err != nil {
				if err != nil {
					logger.Errorf("BUG: unmarshal %s to yaml failed: %v", value, err)
					return true
				}
			}
		}
		return fn(event, serviceSpec)
	}

	return inf.onSpecPart(storeKey, watcherKey, gjsonPath, specFunc)
}

func (inf *meshInformer) StopWatchServiceSpec(serviceName string, gjsonPath GJSONPath) {
	watcherKey := serviceSpecWatcherKey(serviceName, gjsonPath)
	inf.stopWatchOneKey(watcherKey)
}

// OnPartOfInstanceSpec watches one service's instance spec by given gjsonPath.
func (inf *meshInformer) OnPartOfInstanceSpec(serviceName, instanceID string, gjsonPath GJSONPath, fn ServicesInstanceSpecFunc) error {
	storeKey := layout.ServiceInstanceSpecKey(serviceName, instanceID)
	watcherKey := fmt.Sprintf("service-instance-spec-%s-%s-%s", serviceName, instanceID, gjsonPath)

	specFunc := func(event Event, value string) bool {
		instanceSpec := &spec.ServiceInstanceSpec{}
		if event.EventType != EventDelete {
			if err := yaml.Unmarshal([]byte(value), instanceSpec); err != nil {
				if err != nil {
					logger.Errorf("BUG: unmarshal %s to yaml failed: %v", value, err)
					return true
				}
			}
		}
		return fn(event, instanceSpec)
	}

	return inf.onSpecPart(storeKey, watcherKey, gjsonPath, specFunc)
}

// OnPartOfServiceInstanceStatus watches one service instance status spec by given gjsonPath.
func (inf *meshInformer) OnPartOfServiceInstanceStatus(serviceName, instanceID string, gjsonPath GJSONPath, fn ServiceInstanceStatusFunc) error {
	storeKey := layout.ServiceInstanceStatusKey(serviceName, instanceID)
	watcherKey := fmt.Sprintf("service-instance-status-%s-%s-%s", serviceName, instanceID, gjsonPath)

	specFunc := func(event Event, value string) bool {
		instanceStatus := &spec.ServiceInstanceStatus{}
		if event.EventType != EventDelete {
			if err := yaml.Unmarshal([]byte(value), instanceStatus); err != nil {
				if err != nil {
					logger.Errorf("BUG: unmarshal %s to yaml failed: %v", value, err)
					return true
				}
			}
		}
		return fn(event, instanceStatus)
	}

	return inf.onSpecPart(storeKey, watcherKey, gjsonPath, specFunc)
}

// OnPartOfTenantSpec watches one tenant status spec by given gjsonPath.
func (inf *meshInformer) OnPartOfTenantSpec(tenant string, gjsonPath GJSONPath, fn TenantSpecFunc) error {
	storeKey := layout.TenantSpecKey(tenant)
	watcherKey := fmt.Sprintf("tenant-%s", tenant)

	specFunc := func(event Event, value string) bool {
		tenantSpec := &spec.Tenant{}
		if event.EventType != EventDelete {
			if err := yaml.Unmarshal([]byte(value), tenantSpec); err != nil {
				if err != nil {
					logger.Errorf("BUG: unmarshal %s to yaml failed: %v", value, err)
					return true
				}
			}
		}
		return fn(event, tenantSpec)
	}

	return inf.onSpecPart(storeKey, watcherKey, gjsonPath, specFunc)
}

// OnPartOfIngressSpec watches one ingress status spec by given gjsonPath.
func (inf *meshInformer) OnPartOfIngressSpec(ingress string, gjsonPath GJSONPath, fn IngressSpecFunc) error {
	storeKey := layout.IngressSpecKey(ingress)
	watcherKey := fmt.Sprintf("ingress-%s", ingress)

	specFunc := func(event Event, value string) bool {
		ingressSpec := &spec.Ingress{}
		if event.EventType != EventDelete {
			if err := yaml.Unmarshal([]byte(value), ingressSpec); err != nil {
				if err != nil {
					logger.Errorf("BUG: unmarshal %s to yaml failed: %v", value, err)
					return true
				}
			}
		}
		return fn(event, ingressSpec)
	}

	return inf.onSpecPart(storeKey, watcherKey, gjsonPath, specFunc)
}

// OnServiceSpecs watches service specs with the prefix.
func (inf *meshInformer) OnServiceSpecs(servicePrefix string, fn ServiceSpecsFunc) error {
	watcherKey := fmt.Sprintf("prefix-service-%s", servicePrefix)

	specsFunc := func(kvs map[string]string) bool {
		services := make(map[string]*spec.Service)
		for k, v := range kvs {
			service := &spec.Service{}
			if err := yaml.Unmarshal([]byte(v), service); err != nil {
				logger.Errorf("BUG: unmarshal %s to yaml failed: %v", v, err)
				continue
			}
			services[k] = service
		}

		return fn(services)
	}

	return inf.onSpecs(servicePrefix, watcherKey, specsFunc)
}

func serviceInstanceSpecWatcherKey(serviceName string) string {
	return fmt.Sprintf("prefix-service-instance-spec-%s", serviceName)
}

// OnServiceInstanceSpecs watches one service all instance specs.
func (inf *meshInformer) OnServiceInstanceSpecs(serviceName string, fn ServiceInstanceSpecsFunc) error {
	instancePrefix := layout.ServiceInstanceSpecPrefix(serviceName)
	watcherKey := serviceInstanceSpecWatcherKey(serviceName)

	specsFunc := func(kvs map[string]string) bool {
		instanceSpecs := make(map[string]*spec.ServiceInstanceSpec)
		for k, v := range kvs {
			instanceSpec := &spec.ServiceInstanceSpec{}
			if err := yaml.Unmarshal([]byte(v), instanceSpec); err != nil {
				logger.Errorf("BUG: unmarshal %s to yaml failed: %v", v, err)
				continue
			}
			instanceSpecs[k] = instanceSpec
		}

		return fn(instanceSpecs)
	}

	return inf.onSpecs(instancePrefix, watcherKey, specsFunc)
}

func (inf *meshInformer) StopWatchServiceInstanceSpec(serviceName string) {
	watcherKey := serviceInstanceSpecWatcherKey(serviceName)
	inf.stopWatchOneKey(watcherKey)
}

// OnServiceInstanceStatuses watches service instance statuses with the same prefix.
func (inf *meshInformer) OnServiceInstanceStatuses(serviceName string, fn ServiceInstanceStatusesFunc) error {
	watcherKey := fmt.Sprintf("prefix-service-instance-status-%s", serviceName)
	instanceStatusPrefix := layout.ServiceInstanceStatusPrefix(serviceName)

	specsFunc := func(kvs map[string]string) bool {
		instanceStatuses := make(map[string]*spec.ServiceInstanceStatus)
		for k, v := range kvs {
			instanceStatus := &spec.ServiceInstanceStatus{}
			if err := yaml.Unmarshal([]byte(v), instanceStatus); err != nil {
				logger.Errorf("BUG: unmarshal %s to yaml failed: %v", v, err)
				continue
			}
			instanceStatuses[k] = instanceStatus
		}

		return fn(instanceStatuses)
	}

	return inf.onSpecs(instanceStatusPrefix, watcherKey, specsFunc)
}

// OnTenantSpecs watches tenant specs with the same prefix.
func (inf *meshInformer) OnTenantSpecs(tenantPrefix string, fn TenantSpecsFunc) error {
	watcherKey := fmt.Sprintf("prefix-tenant-%s", tenantPrefix)

	specsFunc := func(kvs map[string]string) bool {
		tenants := make(map[string]*spec.Tenant)
		for k, v := range kvs {
			tenantSpec := &spec.Tenant{}
			if err := yaml.Unmarshal([]byte(v), tenantSpec); err != nil {
				logger.Errorf("BUG: unmarshal %s to yaml failed: %v", v, err)
				continue
			}
			tenants[k] = tenantSpec
		}

		return fn(tenants)
	}

	return inf.onSpecs(tenantPrefix, watcherKey, specsFunc)
}

// OnIngressSpecs watches ingress specs
func (inf *meshInformer) OnIngressSpecs(fn IngressSpecsFunc) error {
	storeKey := layout.IngressPrefix()
	watcherKey := "prefix-ingress"

	specsFunc := func(kvs map[string]string) bool {
		ingresss := make(map[string]*spec.Ingress)
		for k, v := range kvs {
			ingressSpec := &spec.Ingress{}
			if err := yaml.Unmarshal([]byte(v), ingressSpec); err != nil {
				logger.Errorf("BUG: unmarshal %s to yaml failed: %v", v, err)
				continue
			}
			ingresss[k] = ingressSpec
		}

		return fn(ingresss)
	}

	return inf.onSpecs(storeKey, watcherKey, specsFunc)
}

func (inf *meshInformer) comparePart(path GJSONPath, old, new string) bool {
	if path == AllParts {
		return old == new
	}

	oldJSON, err := yamljsontool.YAMLToJSON([]byte(old))
	if err != nil {
		logger.Errorf("BUG: transform yaml %s to json failed: %v", old, err)
		return true
	}

	newJSON, err := yamljsontool.YAMLToJSON([]byte(new))
	if err != nil {
		logger.Errorf("BUG: transform yaml %s to json failed: %v", new, err)
		return true
	}

	return gjson.Get(string(oldJSON), string(path)) == gjson.Get(string(newJSON), string(path))
}

func (inf *meshInformer) onSpecPart(storeKey, watcherKey string, gjsonPath GJSONPath, fn specHandleFunc) error {
	inf.mutex.Lock()
	defer inf.mutex.Unlock()

	if inf.closed {
		return ErrClosed
	}

	if _, ok := inf.watchers[watcherKey]; ok {
		logger.Infof("watch key: %s already", watcherKey)
		return ErrAlreadyWatched
	}

	kv, err := inf.store.GetRaw(storeKey)
	if err != nil {
		return err
	}
	if kv == nil {
		return ErrNotFound
	}

	watcher, err := inf.store.Watcher()
	if err != nil {
		return err
	}

	ch, err := watcher.WatchRawFromRev(storeKey, kv.ModRevision)
	if err != nil {
		return err
	}

	inf.watchers[watcherKey] = watcher

	go inf.watch(ch, watcherKey, gjsonPath, fn)

	return nil
}

func (inf *meshInformer) onSpecs(storePrefix, watcherKey string, fn specsHandleFunc) error {
	inf.mutex.Lock()
	defer inf.mutex.Unlock()

	if inf.closed {
		return ErrClosed
	}

	if _, exists := inf.watchers[watcherKey]; exists {
		logger.Infof("watch prefix:%s already", watcherKey)
		return ErrAlreadyWatched
	}

	kvs, err := inf.store.GetRawPrefix(storePrefix)
	if err != nil {
		return err
	}

	watcher, err := inf.store.Watcher()
	if err != nil {
		return err
	}

	minRev := int64(^uint64(0) >> 1)
	for _, v := range kvs {
		if v.ModRevision < minRev {
			minRev = v.ModRevision
		}
	}
	ch, err := watcher.WatchRawPrefixFromRev(storePrefix, minRev)
	if err != nil {
		return err
	}

	inf.watchers[watcherKey] = watcher

	go inf.watchPrefix(ch, watcherKey, fn)

	return nil
}

func (inf *meshInformer) Close() {
	inf.mutex.Lock()
	defer inf.mutex.Unlock()

	for _, watcher := range inf.watchers {
		watcher.Close()
	}

	inf.closed = true
}

func (inf *meshInformer) watch(ch <-chan *clientv3.Event, watcherKey string, path GJSONPath, fn specHandleFunc) {
	event := <-ch
	oldValue := string(event.Kv.Value)
	if !fn(Event{EventType: EventUpdate, RawKV: event.Kv}, oldValue) {
		inf.stopWatchOneKey(watcherKey)
	}

	for event = range ch {
		continueWatch := true
		if event == nil {
			continueWatch = fn(Event{EventType: EventDelete}, "")
		} else {
			newValue := string(event.Kv.Value)
			if !inf.comparePart(path, oldValue, newValue) {
				continueWatch = fn(Event{EventType: EventUpdate, RawKV: event.Kv}, newValue)
			}
			oldValue = newValue
		}

		if !continueWatch {
			inf.stopWatchOneKey(watcherKey)
		}
	}
}

func (inf *meshInformer) watchPrefix(ch <-chan map[string]*clientv3.Event, watcherKey string, fn specsHandleFunc) {
	kvs := make(map[string]string)

	changedKVs := <-ch
	for k, v := range changedKVs {
		if v != nil {
			kvs[k] = string(v.Kv.Value)
		}
	}

	if !fn(kvs) {
		inf.stopWatchOneKey(watcherKey)
	}

	for changedKVs = range ch {
		changed := false

		for k, v := range changedKVs {
			if v == nil {
				delete(kvs, k)
				changed = true
				logger.Infof("delete record: %s", k)
			} else {
				if oldValue, ok := kvs[k]; ok {
					if oldValue == string(v.Kv.Value) {
						continue
					}
				}
				kvs[k] = string(v.Kv.Value)
				changed = true
				logger.Infof("update record, update: %s, version: %d", k, v.Kv.Version)
			}
		}

		if changed && !fn(kvs) {
			inf.stopWatchOneKey(watcherKey)
		}
	}
}