package api

import (
	"strings"
	"testing"

	"alert-system/internal/domain"
)

func TestIncidentForResponseMasksCommunicationTarget(t *testing.T) {
	original := domain.Incident{
		CommunicationChannel:   "lark_webhook",
		CommunicationTargetRef: "https://open.feishu.cn/open-apis/bot/v2/hook/secret-token",
	}
	response := incidentForResponse(original)
	if response.CommunicationTargetRef != "https://open.feishu.cn" {
		t.Fatalf("脱敏目标=%q", response.CommunicationTargetRef)
	}
	if strings.Contains(response.CommunicationTargetRef, "secret-token") {
		t.Fatal("Incident API 响应泄露 webhook token")
	}
	if original.CommunicationTargetRef == response.CommunicationTargetRef {
		t.Fatal("脱敏函数不应修改或返回原始敏感目标")
	}
}
