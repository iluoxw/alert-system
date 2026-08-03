package pipeline

// ConfigCacheInvalidator 将配置 API 与管道内部缓存隔离：API 只依赖两个失效方法，
// 不需要知道 RouteStage/SilenceStage 的实现与锁。
type ConfigCacheInvalidator struct {
	routes   *RouteStage
	silences *SilenceStage
}

func NewConfigCacheInvalidator(routes *RouteStage, silences *SilenceStage) *ConfigCacheInvalidator {
	return &ConfigCacheInvalidator{routes: routes, silences: silences}
}

func (i *ConfigCacheInvalidator) InvalidateRoutes() {
	if i != nil && i.routes != nil {
		i.routes.InvalidateCache()
	}
}

func (i *ConfigCacheInvalidator) InvalidateSilences() {
	if i != nil && i.silences != nil {
		i.silences.InvalidateCache()
	}
}
