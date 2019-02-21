// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/model"

	"github.com/prometheus/statsd_exporter/pkg/clock"
	"github.com/prometheus/statsd_exporter/pkg/mapper"
)

const (
	defaultHelp = "Metric autogenerated by statsd_exporter."
	regErrF     = "A change of configuration created inconsistent metrics for " +
		"%q. You have to restart the statsd_exporter, and you should " +
		"consider the effects on your monitoring setup. Error: %s"
)

var (
	illegalCharsRE = regexp.MustCompile(`[^a-zA-Z0-9_]`)

	hash   = fnv.New64a()
	strBuf bytes.Buffer // Used for hashing.
	intBuf = make([]byte, 8)
)

func labelNames(labels prometheus.Labels) []string {
	names := make([]string, 0, len(labels))
	for labelName := range labels {
		names = append(names, labelName)
	}
	sort.Strings(names)
	return names
}

// hashNameAndLabels returns a hash value of the provided name string and all
// the label names and values in the provided labels map.
//
// Not safe for concurrent use! (Uses a shared buffer and hasher to save on
// allocations.)
func hashNameAndLabels(name string, labels prometheus.Labels) uint64 {
	hash.Reset()
	strBuf.Reset()
	strBuf.WriteString(name)
	hash.Write(strBuf.Bytes())
	binary.BigEndian.PutUint64(intBuf, model.LabelsToSignature(labels))
	hash.Write(intBuf)
	return hash.Sum64()
}

type CounterContainer struct {
	//           metric name
	Elements map[string]*prometheus.CounterVec
}

func NewCounterContainer() *CounterContainer {
	return &CounterContainer{
		Elements: make(map[string]*prometheus.CounterVec),
	}
}

func (c *CounterContainer) Get(metricName string, labels prometheus.Labels, help string) (prometheus.Counter, error) {
	counterVec, ok := c.Elements[metricName]
	if !ok {
		counterVec = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricName,
			Help: help,
		}, labelNames(labels))
		if err := prometheus.Register(counterVec); err != nil {
			return nil, err
		}
		c.Elements[metricName] = counterVec
	}
	return counterVec.GetMetricWith(labels)
}

func (c *CounterContainer) Delete(metricName string, labels prometheus.Labels) {
	if _, ok := c.Elements[metricName]; ok {
		c.Elements[metricName].Delete(labels)
	}
}

type GaugeContainer struct {
	Elements map[string]*prometheus.GaugeVec
}

func NewGaugeContainer() *GaugeContainer {
	return &GaugeContainer{
		Elements: make(map[string]*prometheus.GaugeVec),
	}
}

func (c *GaugeContainer) Get(metricName string, labels prometheus.Labels, help string) (prometheus.Gauge, error) {
	gaugeVec, ok := c.Elements[metricName]
	if !ok {
		gaugeVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricName,
			Help: help,
		}, labelNames(labels))
		if err := prometheus.Register(gaugeVec); err != nil {
			return nil, err
		}
		c.Elements[metricName] = gaugeVec
	}
	return gaugeVec.GetMetricWith(labels)
}

func (c *GaugeContainer) Delete(metricName string, labels prometheus.Labels) {
	if _, ok := c.Elements[metricName]; ok {
		c.Elements[metricName].Delete(labels)
	}
}

type SummaryContainer struct {
	Elements map[string]*prometheus.SummaryVec
	mapper   *mapper.MetricMapper
}

func NewSummaryContainer(mapper *mapper.MetricMapper) *SummaryContainer {
	return &SummaryContainer{
		Elements: make(map[string]*prometheus.SummaryVec),
		mapper:   mapper,
	}
}

func (c *SummaryContainer) Get(metricName string, labels prometheus.Labels, help string, mapping *mapper.MetricMapping) (prometheus.Observer, error) {
	summaryVec, ok := c.Elements[metricName]
	if !ok {
		quantiles := c.mapper.Defaults.Quantiles
		if mapping != nil && mapping.Quantiles != nil && len(mapping.Quantiles) > 0 {
			quantiles = mapping.Quantiles
		}
		objectives := make(map[float64]float64)
		for _, q := range quantiles {
			objectives[q.Quantile] = q.Error
		}
		summaryVec = prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       metricName,
				Help:       help,
				Objectives: objectives,
			}, labelNames(labels))
		if err := prometheus.Register(summaryVec); err != nil {
			return nil, err
		}
		c.Elements[metricName] = summaryVec
	}
	return summaryVec.GetMetricWith(labels)
}

func (c *SummaryContainer) Delete(metricName string, labels prometheus.Labels) {
	if _, ok := c.Elements[metricName]; ok {
		c.Elements[metricName].Delete(labels)
	}
}

type HistogramContainer struct {
	Elements map[string]*prometheus.HistogramVec
	mapper   *mapper.MetricMapper
}

func NewHistogramContainer(mapper *mapper.MetricMapper) *HistogramContainer {
	return &HistogramContainer{
		Elements: make(map[string]*prometheus.HistogramVec),
		mapper:   mapper,
	}
}

func (c *HistogramContainer) Get(metricName string, labels prometheus.Labels, help string, mapping *mapper.MetricMapping) (prometheus.Observer, error) {
	histogramVec, ok := c.Elements[metricName]
	if !ok {
		buckets := c.mapper.Defaults.Buckets
		if mapping != nil && mapping.Buckets != nil && len(mapping.Buckets) > 0 {
			buckets = mapping.Buckets
		}
		histogramVec = prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    metricName,
				Help:    help,
				Buckets: buckets,
			}, labelNames(labels))
		if err := prometheus.Register(histogramVec); err != nil {
			return nil, err
		}
		c.Elements[metricName] = histogramVec
	}
	return histogramVec.GetMetricWith(labels)
}

func (c *HistogramContainer) Delete(metricName string, labels prometheus.Labels) {
	if _, ok := c.Elements[metricName]; ok {
		c.Elements[metricName].Delete(labels)
	}
}

type Event interface {
	MetricName() string
	Value() float64
	Labels() map[string]string
	MetricType() mapper.MetricType
}

type CounterEvent struct {
	metricName string
	value      float64
	labels     map[string]string
}

func (c *CounterEvent) MetricName() string            { return c.metricName }
func (c *CounterEvent) Value() float64                { return c.value }
func (c *CounterEvent) Labels() map[string]string     { return c.labels }
func (c *CounterEvent) MetricType() mapper.MetricType { return mapper.MetricTypeCounter }

type GaugeEvent struct {
	metricName string
	value      float64
	relative   bool
	labels     map[string]string
}

func (g *GaugeEvent) MetricName() string            { return g.metricName }
func (g *GaugeEvent) Value() float64                { return g.value }
func (c *GaugeEvent) Labels() map[string]string     { return c.labels }
func (c *GaugeEvent) MetricType() mapper.MetricType { return mapper.MetricTypeGauge }

type TimerEvent struct {
	metricName string
	value      float64
	labels     map[string]string
}

func (t *TimerEvent) MetricName() string            { return t.metricName }
func (t *TimerEvent) Value() float64                { return t.value }
func (c *TimerEvent) Labels() map[string]string     { return c.labels }
func (c *TimerEvent) MetricType() mapper.MetricType { return mapper.MetricTypeTimer }

type Events []Event

type LabelValues struct {
	lastRegisteredAt time.Time
	labels           prometheus.Labels
	ttl              time.Duration
}

type Exporter struct {
	Counters    *CounterContainer
	Gauges      *GaugeContainer
	Summaries   *SummaryContainer
	Histograms  *HistogramContainer
	mapper      *mapper.MetricMapper
	labelValues map[string]map[uint64]*LabelValues
}

func escapeMetricName(metricName string) string {
	// If a metric starts with a digit, prepend an underscore.
	if metricName[0] >= '0' && metricName[0] <= '9' {
		metricName = "_" + metricName
	}

	// Replace all illegal metric chars with underscores.
	metricName = illegalCharsRE.ReplaceAllString(metricName, "_")
	return metricName
}

// Listen handles all events sent to the given channel sequentially. It
// terminates when the channel is closed.
func (b *Exporter) Listen(e <-chan Events) {
	removeStaleMetricsTicker := clock.NewTicker(time.Second)

	for {
		select {
		case <-removeStaleMetricsTicker.C:
			b.removeStaleMetrics()
		case events, ok := <-e:
			if !ok {
				log.Debug("Channel is closed. Break out of Exporter.Listener.")
				removeStaleMetricsTicker.Stop()
				return
			}
			for _, event := range events {
				b.handleEvent(event)
			}
		}
	}
}

// handleEvent processes a single Event according to the configured mapping.
func (b *Exporter) handleEvent(event Event) {
	mapping, labels, present := b.mapper.GetMapping(event.MetricName(), event.MetricType())
	if mapping == nil {
		mapping = &mapper.MetricMapping{}
		if b.mapper.Defaults.Ttl != 0 {
			mapping.Ttl = b.mapper.Defaults.Ttl
		}
	}

	if mapping.Action == mapper.ActionTypeDrop {
		return
	}

	help := defaultHelp
	if mapping.HelpText != "" {
		help = mapping.HelpText
	}

	metricName := ""
	prometheusLabels := event.Labels()
	if present {
		metricName = escapeMetricName(mapping.Name)
		for label, value := range labels {
			prometheusLabels[label] = value
		}
	} else {
		eventsUnmapped.Inc()
		metricName = escapeMetricName(event.MetricName())
	}

	switch ev := event.(type) {
	case *CounterEvent:
		// We don't accept negative values for counters. Incrementing the counter with a negative number
		// will cause the exporter to panic. Instead we will warn and continue to the next event.
		if event.Value() < 0.0 {
			log.Debugf("Counter %q is: '%f' (counter must be non-negative value)", metricName, event.Value())
			eventStats.WithLabelValues("illegal_negative_counter").Inc()
			return
		}

		counter, err := b.Counters.Get(
			metricName,
			prometheusLabels,
			help,
		)
		if err == nil {
			counter.Add(event.Value())
			b.saveLabelValues(metricName, prometheusLabels, mapping.Ttl)
			eventStats.WithLabelValues("counter").Inc()
		} else {
			log.Debugf(regErrF, metricName, err)
			conflictingEventStats.WithLabelValues("counter").Inc()
		}

	case *GaugeEvent:
		gauge, err := b.Gauges.Get(
			metricName,
			prometheusLabels,
			help,
		)

		if err == nil {
			if ev.relative {
				gauge.Add(event.Value())
			} else {
				gauge.Set(event.Value())
			}
			b.saveLabelValues(metricName, prometheusLabels, mapping.Ttl)
			eventStats.WithLabelValues("gauge").Inc()
		} else {
			log.Debugf(regErrF, metricName, err)
			conflictingEventStats.WithLabelValues("gauge").Inc()
		}

	case *TimerEvent:
		t := mapper.TimerTypeDefault
		if mapping != nil {
			t = mapping.TimerType
		}
		if t == mapper.TimerTypeDefault {
			t = b.mapper.Defaults.TimerType
		}

		switch t {
		case mapper.TimerTypeHistogram:
			histogram, err := b.Histograms.Get(
				metricName,
				prometheusLabels,
				help,
				mapping,
			)
			if err == nil {
				histogram.Observe(event.Value() / 1000) // prometheus presumes seconds, statsd millisecond
				b.saveLabelValues(metricName, prometheusLabels, mapping.Ttl)
				eventStats.WithLabelValues("timer").Inc()
			} else {
				log.Debugf(regErrF, metricName, err)
				conflictingEventStats.WithLabelValues("timer").Inc()
			}

		case mapper.TimerTypeDefault, mapper.TimerTypeSummary:
			summary, err := b.Summaries.Get(
				metricName,
				prometheusLabels,
				help,
				mapping,
			)
			if err == nil {
				summary.Observe(event.Value() / 1000) // prometheus presumes seconds, statsd millisecond
				b.saveLabelValues(metricName, prometheusLabels, mapping.Ttl)
				eventStats.WithLabelValues("timer").Inc()
			} else {
				log.Debugf(regErrF, metricName, err)
				conflictingEventStats.WithLabelValues("timer").Inc()
			}

		default:
			panic(fmt.Sprintf("unknown timer type '%s'", t))
		}

	default:
		log.Debugln("Unsupported event type")
		eventStats.WithLabelValues("illegal").Inc()
	}
}

// removeStaleMetrics removes label values set from metric with stale values
func (b *Exporter) removeStaleMetrics() {
	now := clock.Now()
	// delete timeseries with expired ttl
	for metricName := range b.labelValues {
		for hash, lvs := range b.labelValues[metricName] {
			if lvs.ttl == 0 {
				continue
			}
			if lvs.lastRegisteredAt.Add(lvs.ttl).Before(now) {
				b.Counters.Delete(metricName, lvs.labels)
				b.Gauges.Delete(metricName, lvs.labels)
				b.Summaries.Delete(metricName, lvs.labels)
				b.Histograms.Delete(metricName, lvs.labels)
				delete(b.labelValues[metricName], hash)
			}
		}
	}
}

// saveLabelValues stores label values set to labelValues and update lastRegisteredAt time and ttl value
func (b *Exporter) saveLabelValues(metricName string, labels prometheus.Labels, ttl time.Duration) {
	metric, hasMetric := b.labelValues[metricName]
	if !hasMetric {
		metric = make(map[uint64]*LabelValues)
		b.labelValues[metricName] = metric
	}
	hash := hashNameAndLabels(metricName, labels)
	metricLabelValues, ok := metric[hash]
	if !ok {
		metricLabelValues = &LabelValues{
			labels: labels,
			ttl:    ttl,
		}
		b.labelValues[metricName][hash] = metricLabelValues
	}
	now := clock.Now()
	metricLabelValues.lastRegisteredAt = now
	// Update ttl from mapping
	metricLabelValues.ttl = ttl
}

func NewExporter(mapper *mapper.MetricMapper) *Exporter {
	return &Exporter{
		Counters:    NewCounterContainer(),
		Gauges:      NewGaugeContainer(),
		Summaries:   NewSummaryContainer(mapper),
		Histograms:  NewHistogramContainer(mapper),
		mapper:      mapper,
		labelValues: make(map[string]map[uint64]*LabelValues),
	}
}

func buildEvent(statType, metric string, value float64, relative bool, labels map[string]string) (Event, error) {
	switch statType {
	case "c":
		return &CounterEvent{
			metricName: metric,
			value:      float64(value),
			labels:     labels,
		}, nil
	case "g":
		return &GaugeEvent{
			metricName: metric,
			value:      float64(value),
			relative:   relative,
			labels:     labels,
		}, nil
	case "ms", "h":
		return &TimerEvent{
			metricName: metric,
			value:      float64(value),
			labels:     labels,
		}, nil
	case "s":
		return nil, fmt.Errorf("no support for StatsD sets")
	default:
		return nil, fmt.Errorf("bad stat type %s", statType)
	}
}

func parseDogStatsDTagsToLabels(component string) map[string]string {
	labels := map[string]string{}
	tagsReceived.Inc()
	tags := strings.Split(component, ",")
	for _, t := range tags {
		t = strings.TrimPrefix(t, "#")
		kv := strings.SplitN(t, ":", 2)

		if len(kv) < 2 || len(kv[1]) == 0 {
			tagErrors.Inc()
			log.Debugf("Malformed or empty DogStatsD tag %s in component %s", t, component)
			continue
		}

		labels[escapeMetricName(kv[0])] = kv[1]
	}
	return labels
}

func lineToEvents(line string) Events {
	events := Events{}
	if line == "" {
		return events
	}

	elements := strings.SplitN(line, ":", 2)
	if len(elements) < 2 || len(elements[0]) == 0 || !utf8.ValidString(line) {
		sampleErrors.WithLabelValues("malformed_line").Inc()
		log.Debugln("Bad line from StatsD:", line)
		return events
	}
	metric := elements[0]
	var samples []string
	if strings.Contains(elements[1], "|#") {
		// using datadog extensions, disable multi-metrics
		samples = elements[1:]
	} else {
		samples = strings.Split(elements[1], ":")
	}
samples:
	for _, sample := range samples {
		samplesReceived.Inc()
		components := strings.Split(sample, "|")
		samplingFactor := 1.0
		if len(components) < 2 || len(components) > 4 {
			sampleErrors.WithLabelValues("malformed_component").Inc()
			log.Debugln("Bad component on line:", line)
			continue
		}
		valueStr, statType := components[0], components[1]

		var relative = false
		if strings.Index(valueStr, "+") == 0 || strings.Index(valueStr, "-") == 0 {
			relative = true
		}

		value, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			log.Debugf("Bad value %s on line: %s", valueStr, line)
			sampleErrors.WithLabelValues("malformed_value").Inc()
			continue
		}

		multiplyEvents := 1
		labels := map[string]string{}
		if len(components) >= 3 {
			for _, component := range components[2:] {
				if len(component) == 0 {
					log.Debugln("Empty component on line: ", line)
					sampleErrors.WithLabelValues("malformed_component").Inc()
					continue samples
				}
			}

			for _, component := range components[2:] {
				switch component[0] {
				case '@':
					if statType != "c" && statType != "ms" {
						log.Debugln("Illegal sampling factor for non-counter metric on line", line)
						sampleErrors.WithLabelValues("illegal_sample_factor").Inc()
						continue
					}
					samplingFactor, err = strconv.ParseFloat(component[1:], 64)
					if err != nil {
						log.Debugf("Invalid sampling factor %s on line %s", component[1:], line)
						sampleErrors.WithLabelValues("invalid_sample_factor").Inc()
					}
					if samplingFactor == 0 {
						samplingFactor = 1
					}

					if statType == "c" {
						value /= samplingFactor
					} else if statType == "ms" {
						multiplyEvents = int(1 / samplingFactor)
					}
				case '#':
					labels = parseDogStatsDTagsToLabels(component)
				default:
					log.Debugf("Invalid sampling factor or tag section %s on line %s", components[2], line)
					sampleErrors.WithLabelValues("invalid_sample_factor").Inc()
					continue
				}
			}
		}

		for i := 0; i < multiplyEvents; i++ {
			event, err := buildEvent(statType, metric, value, relative, labels)
			if err != nil {
				log.Debugf("Error building event on line %s: %s", line, err)
				sampleErrors.WithLabelValues("illegal_event").Inc()
				continue
			}
			events = append(events, event)
		}
	}
	return events
}

type StatsDUDPListener struct {
	conn *net.UDPConn
}

func (l *StatsDUDPListener) Listen(threads string, e chan<- Events) {
	t, err := strconv.Atoi(threads)
	if err != nil {
		log.Error(fmt.Sprintf("Unable to convert thread option %v to int", threads))
		t = 1
	}
	for i := 0; i < t; i++ {
		go l.Listener(e)
	}
}

func (l *StatsDUDPListener) Listener(e chan<- Events) {
	buf := make([]byte, 65535)
	for {
		n, _, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			log.Fatal(err)
		}
		data := append([]byte(nil), buf[0:n]...)
		go l.handlePacket(data[0:n], e)
	}
}

func (l *StatsDUDPListener) handlePacket(packet []byte, e chan<- Events) {
	udpPackets.Inc()
	lines := strings.Split(string(packet), "\n")
	events := Events{}
	for _, line := range lines {
		linesReceived.Inc()
		events = append(events, lineToEvents(line)...)
	}
	e <- events
}

type StatsDTCPListener struct {
	conn *net.TCPListener
}

func (l *StatsDTCPListener) Listen(e chan<- Events) {
	for {
		c, err := l.conn.AcceptTCP()
		if err != nil {
			log.Fatalf("AcceptTCP failed: %v", err)
		}
		go l.handleConn(c, e)
	}
}

func (l *StatsDTCPListener) handleConn(c *net.TCPConn, e chan<- Events) {
	defer c.Close()

	tcpConnections.Inc()

	r := bufio.NewReader(c)
	for {
		line, isPrefix, err := r.ReadLine()
		if err != nil {
			if err != io.EOF {
				tcpErrors.Inc()
				log.Debugf("Read %s failed: %v", c.RemoteAddr(), err)
			}
			break
		}
		if isPrefix {
			tcpLineTooLong.Inc()
			log.Debugf("Read %s failed: line too long", c.RemoteAddr())
			break
		}
		linesReceived.Inc()
		e <- lineToEvents(string(line))
	}
}
