// Copyright 2019 The Prometheus Authors
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

package mapper

import (
	"github.com/hashicorp/golang-lru"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	cacheLength = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "statsd_exporter_cache_length",
			Help: "The count of unique metrics currently cached.",
		},
	)
)

type MetricMapperCacheResult struct {
	Mapping *MetricMapping
	Matched bool
	Labels  prometheus.Labels
}

type MetricMapperCache interface {
	Get(metricString string) (*MetricMapperCacheResult, bool)
	AddMatch(metricString string, mapping *MetricMapping, labels prometheus.Labels)
	AddMiss(metricString string)
}

type MetricMapperLRUCache struct {
	MetricMapperCache
	cache *lru.Cache
}

type MetricMapperNoopCache struct {
	MetricMapperCache
}

func NewMetricMapperCache(size int) (*MetricMapperLRUCache, error) {
	cacheLength.Set(0)
	cache, err := lru.New(size)
	if err != nil {
		return &MetricMapperLRUCache{}, err
	}
	return &MetricMapperLRUCache{cache: cache}, nil
}

func (m *MetricMapperLRUCache) Get(metricString string) (*MetricMapperCacheResult, bool) {
	if result, ok := m.cache.Get(metricString); ok {
		return result.(*MetricMapperCacheResult), true
	} else {
		return nil, false
	}
}

func (m *MetricMapperLRUCache) AddMatch(metricString string, mapping *MetricMapping, labels prometheus.Labels) {
	go m.trackCacheLength()
	m.cache.Add(metricString, &MetricMapperCacheResult{Mapping: mapping, Matched: true, Labels: labels})
}

func (m *MetricMapperLRUCache) AddMiss(metricString string) {
	go m.trackCacheLength()
	m.cache.Add(metricString, &MetricMapperCacheResult{Matched: false})
}

func (m *MetricMapperLRUCache) trackCacheLength() {
	cacheLength.Set(float64(m.cache.Len()))
}

func NewMetricMapperNoopCache() *MetricMapperNoopCache {
	cacheLength.Set(0)
	return &MetricMapperNoopCache{}
}

func (m *MetricMapperNoopCache) Get(metricString string) (*MetricMapperCacheResult, bool) {
	return nil, false
}

func (m *MetricMapperNoopCache) AddMatch(metricString string, mapping *MetricMapping, labels prometheus.Labels) {
	return
}

func (m *MetricMapperNoopCache) AddMiss(metricString string) {
	return
}

func init() {
	prometheus.MustRegister(cacheLength)
}
