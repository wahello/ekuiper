package nodes

import (
	"fmt"
	"github.com/emqx/kuiper/common"
	"github.com/emqx/kuiper/common/plugin_manager"
	"github.com/emqx/kuiper/xsql"
	"github.com/emqx/kuiper/xstream/api"
	"github.com/emqx/kuiper/xstream/extensions"
	"github.com/go-yaml/yaml"
	"sync"
)

type SourceNode struct {
	sourceType  string
	outs        map[string]chan<- interface{}
	name        string
	ctx         api.StreamContext
	options     map[string]string
	concurrency int

	mutex   sync.RWMutex
	sources []api.Source
}

func NewSourceNode(name string, options map[string]string) *SourceNode {
	t, ok := options["TYPE"]
	if !ok {
		t = "mqtt"
	}
	return &SourceNode{
		sourceType:  t,
		outs:        make(map[string]chan<- interface{}),
		name:        name,
		options:     options,
		ctx:         nil,
		concurrency: 1,
	}
}

func (m *SourceNode) Open(ctx api.StreamContext, errCh chan<- error) {
	m.ctx = ctx
	logger := ctx.GetLogger()
	logger.Debugf("open source node %s with option %v", m.name, m.options)
	go func() {
		props := m.getConf(ctx)
		if c, ok := props["concurrency"]; ok {
			if f, ok := c.(float64); ok {
				m.concurrency = int(f)
			}
		}

		for i := 0; i < m.concurrency; i++ { // workers
			go func() {
				//Do open source instances
				source, err := getSource(m.sourceType)
				if err != nil {
					m.drainError(errCh, err, ctx, logger)
					return
				}
				err = source.Configure(m.options["DATASOURCE"], props)
				if err != nil {
					m.drainError(errCh, err, ctx, logger)
					return
				}
				m.mutex.Lock()
				m.sources = append(m.sources, source)
				m.mutex.Unlock()
				if err := source.Open(ctx, func(message map[string]interface{}, meta map[string]interface{}) {
					tuple := &xsql.Tuple{Emitter: m.name, Message: message, Timestamp: common.GetNowInMilli(), Metadata: meta}
					m.Broadcast(tuple)
					logger.Debugf("%s consume data %v complete", m.name, tuple)
				}); err != nil {
					m.drainError(errCh, err, ctx, logger)
					return
				}
			}()
		}

		for {
			select {
			case <-ctx.Done():
				logger.Infof("source %s done", m.name)
				m.close(ctx, logger)
				return
			}
		}
	}()
}

func getSource(t string) (api.Source, error) {
	var s api.Source
	var ok bool
	switch t {
	case "mqtt":
		s = &extensions.MQTTSource{}
	default:
		nf, err := plugin_manager.GetPlugin(t, "sources")
		if err != nil {
			return nil, err
		}
		s, ok = nf.(api.Source)
		if !ok {
			return nil, fmt.Errorf("exported symbol %s is not type of api.Source", t)
		}
	}
	return s, nil
}

func (m *SourceNode) drainError(errCh chan<- error, err error, ctx api.StreamContext, logger api.Logger) {
	select {
	case errCh <- err:
	case <-ctx.Done():
		m.close(ctx, logger)
	}
	return
}

func (m *SourceNode) close(ctx api.StreamContext, logger api.Logger) {
	for _, s := range m.sources {
		if err := s.Close(ctx); err != nil {
			logger.Warnf("close source fails: %v", err)
		}
	}
}

func (m *SourceNode) getConf(ctx api.StreamContext) map[string]interface{} {
	confkey := m.options["CONF_KEY"]
	logger := ctx.GetLogger()
	confPath := "sources/" + m.sourceType + ".yaml"
	if m.sourceType == "mqtt" {
		confPath = "mqtt_source.yaml"
	}
	conf, err := common.LoadConf(confPath)
	props := make(map[string]interface{})
	if err == nil {
		cfg := make(map[string]map[string]interface{})
		if err := yaml.Unmarshal(conf, &cfg); err != nil {
			logger.Warnf("fail to parse yaml for source %s. Return an empty configuration", m.sourceType)
		} else {
			var ok bool
			props, ok = cfg["default"]
			if !ok {
				logger.Warnf("default conf is not found", confkey)
			}
			if c, ok := cfg[confkey]; ok {
				for k, v := range c {
					props[k] = v
				}
			}
		}
	} else {
		logger.Warnf("config file %s.yaml is not loaded properly. Return an empty configuration", m.sourceType)
	}
	logger.Debugf("get conf for %s with conf key %s: %v", m.sourceType, confkey, props)
	return props
}

func (m *SourceNode) Broadcast(data interface{}) int {
	return Broadcast(m.outs, data, m.ctx)
}

func (m *SourceNode) GetName() string {
	return m.name
}

func (m *SourceNode) AddOutput(output chan<- interface{}, name string) (err error) {
	if _, ok := m.outs[name]; !ok {
		m.outs[name] = output
	} else {
		return fmt.Errorf("fail to add output %s, stream node %s already has an output of the same name", name, m.name)
	}
	return nil
}
