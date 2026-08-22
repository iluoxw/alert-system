export type HealthState = 'healthy' | 'degraded' | 'critical' | 'unknown' | 'neutral'
export type DataState = 'complete' | 'partial' | 'stale' | 'unconfigured'
export type AvailabilityState = 'available' | 'empty' | 'unknown' | 'forbidden' | 'unavailable' | 'expired'
export type FreshnessState = 'fresh' | 'stale' | 'unknown'

export interface ServiceIdentity {
  service_id: string
  entity_id: string
  entity_key: string
  name: string
  description: string
  layer: string
  kind: string
  environment: string
  owner_team_id: string
  owner_team_name: string
  tier: string
}

export interface ProfileSummary {
  profile_key: string
  profile_version: number
  status: string
}

export interface HealthSummary {
  status: Exclude<HealthState, 'neutral'>
  reason_codes: string[]
  evaluated_at: string
}

export interface DataSourceStatus {
  source_key: string
  state: AvailabilityState
  freshness: FreshnessState
  observed_at?: string
  retryable: boolean
  reason_code?: string
}

export interface DataStatus {
  state: DataState
  sources: DataSourceStatus[]
}

export interface SignalValue {
  binding_key: string
  label: string
  family: 'golden' | 'red' | 'use'
  semantic: 'latency' | 'traffic' | 'errors' | 'saturation' | 'rate' | 'duration' | 'utilization'
  scope_kind: string
  scope_ref: string
  value: number | null
  display_value: string
  unit: string
  aggregation: string
  window_seconds: number
  observed_at?: string
  freshness: FreshnessState
  availability: AvailabilityState
  health: HealthState
  source_key: string
  baseline_delta_percent?: number
  threshold_source?: string
  measurement_role?: 'direct' | 'proxy'
  proxy_reason?: string
  unknown_reason?: string
}

export interface DomainCounts {
  active_faults: number
  confirmed_impacts: number
  active_incidents: number
}

export interface DirectoryItem {
  service: ServiceIdentity
  profile: ProfileSummary | null
  health: HealthSummary
  data_status: DataStatus
  golden_signals: SignalValue[]
  domain_counts: DomainCounts
  links?: ServiceLink[]
  observed_at: string
}

export interface ServiceDirectoryView {
  schema_version: string
  view_kind: 'service_directory'
  request_id: string
  assembled_at: string
  summary: {
    managed: number
    critical: number
    partial: number
    unconfigured: number
  }
  items: DirectoryItem[]
  next_cursor: string | null
}

export interface FaultSummary {
  fault_id: string
  title: string
  state: string
  severity: string
  owner_team_name: string
  duration_seconds: number
  confirmed_impact_count: number
  potential_impact_count: number
  updated_at: string
}

export interface DependencySummary {
  entity_id: string
  name: string
  layer: string
  group_key: string
  group_label: string
  role: string
  provenance: string
  health: Exclude<HealthState, 'neutral'>
  freshness: FreshnessState
  observed_at: string
}

export interface TimelineEvent {
  event_id: string
  kind: string
  title: string
  source: string
  occurred_at: string
  entity_ids?: string[]
  relation_ids?: string[]
  domain_ref?: string
}

export interface FocusKind {
  key: string
  label: string
  placeholder: string
  value_pattern: string
  primary: boolean
}

export interface ServiceLink {
  kind: string
  label: string
  href: string
}

export interface SectionStatus {
  section_key: string
  state: AvailabilityState
  retryable: boolean
  reason_code?: string
}

export interface ServiceOverviewView {
  schema_version: string
  view_kind: 'service_overview'
  request_id: string
  assembled_at: string
  service: ServiceIdentity
  profile: ProfileSummary | null
  health: HealthSummary
  data_status: DataStatus
  golden_signals: SignalValue[]
  domain_counts: DomainCounts
  faults: FaultSummary[]
  dependencies: DependencySummary[]
  recent_events: TimelineEvent[]
  focus_kinds: FocusKind[]
  links: ServiceLink[]
  sections: SectionStatus[]
}

export interface WorkspaceScope {
  tenant: string
  environment: string
  from: string
  to: string
  timezone: string
}

export interface WorkspaceFocus {
  kind: string
  value: string
  journey: string
  stage?: string
}

export interface JourneyStage {
  key: string
  label: string
  state: 'ok' | 'failed' | 'pending' | 'unknown'
  entity_ids: string[]
  relation_ids: string[]
  evidence_ids: string[]
  reason_code?: string
}

export interface TopologyNode {
  entity_id: string
  name: string
  layer: string
  kind: string
  health: Exclude<HealthState, 'neutral'>
  freshness: FreshnessState
  signals: SignalValue[]
}

export interface TopologyRelation {
  relation_id: string
  source_entity_id: string
  target_entity_id: string
  kind: string
  provenance: 'declared' | 'observed' | 'declared_and_observed' | 'inferred'
  health: Exclude<HealthState, 'neutral'>
  freshness: FreshnessState
  signals: SignalValue[]
}

export interface DiagnosticSummary {
  level: 'observation' | 'correlation' | 'candidate_cause' | 'confirmed_cause' | 'incomplete'
  title: string
  statements: string[]
  evidence_ids: string[]
  missing_requirements: string[]
}

export interface EvidenceRef {
  schema_version: string
  evidence_id: string
  source: string
  kind: string
  locator: {
    provider: string
    dataset: string
    record_id: string
    deep_link: string
  }
  correlation: {
    service_id: string
    focus: { kind: string; value: string }
    entity_ids: string[]
    relation_ids: string[]
    alert_ids?: string[]
  }
  observed_at: string
  fetched_at: string
  expires_at: string
  freshness: FreshnessState
  availability: AvailabilityState
  summary: string
  integrity: { algorithm: string; digest: string }
  sensitivity: string
  redactions: string[]
}

export interface DomainRef {
  id: string
  state: string
  updated_at: string
}

export interface WorkspaceCapabilities {
  view_metrics: boolean
  view_logs: boolean
  view_traces: boolean
  view_domain: boolean
  open_evidence_deep_links: boolean
}

export interface ObservabilityWorkspaceView {
  schema_version: string
  view_kind: 'observability_workspace'
  request_id: string
  context_version: number
  assembled_at: string
  service: ServiceIdentity
  profile: ProfileSummary
  scope: WorkspaceScope
  focus: WorkspaceFocus
  health: HealthSummary
  data_status: DataStatus
  golden_signals: SignalValue[]
  journey: JourneyStage[]
  topology: {
    snapshot_id: string
    partial: boolean
    nodes: TopologyNode[]
    relations: TopologyRelation[]
  }
  diagnostic_summary: DiagnosticSummary
  evidence_refs: EvidenceRef[]
  domain_state: {
    alerts: DomainRef[]
    faults: DomainRef[]
    impacts: DomainRef[]
    incidents: DomainRef[]
  }
  timeline: TimelineEvent[]
  capabilities: WorkspaceCapabilities
  sections: SectionStatus[]
}

export type JsonPatchOperation =
  | { op: 'add' | 'replace'; path: string; value: unknown }
  | { op: 'remove'; path: string }

export interface WorkspaceScenarioFixture {
  key: string
  label: string
  visual_ref?: string
  patch: JsonPatchOperation[]
  assertions: Array<{ path: string; equals?: unknown; length?: number }>
}

export interface WorkspaceScenarios {
  schema_version: string
  description: string
  base_fixture: string
  scenarios: WorkspaceScenarioFixture[]
}
