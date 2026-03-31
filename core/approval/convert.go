package approval

import (
	api "github.com/ALRubinger/aileron/core/api/gen"
)

func toAPIApproval(a Approval) api.Approval {
	var approvers []api.ApprovalActor
	for _, actor := range a.Approvers {
		status := api.ApprovalActorStatus(actor.Status)
		name := actor.DisplayName
		role := actor.Role
		approvers = append(approvers, api.ApprovalActor{
			PrincipalId: actor.PrincipalID,
			DisplayName: &name,
			Role:        &role,
			Status:      &status,
		})
	}

	apiApproval := api.Approval{
		ApprovalId:     a.ApprovalID,
		IntentId:       a.IntentID,
		Status:         statusToAPIStatus(a.Status),
		Approvers:      approvers,
		EditableBounds: toMapPtr(a.EditableBounds),
		ExpiresAt:      a.ExpiresAt,
		RequestedAt:    a.RequestedAt,
		ResolvedAt:     a.ResolvedAt,
	}
	if a.WorkspaceID != "" {
		apiApproval.WorkspaceId = &a.WorkspaceID
	}
	if a.Rationale != "" {
		apiApproval.Rationale = &a.Rationale
	}
	return apiApproval
}

func fromAPIApproval(a api.Approval) Approval {
	var actors []ApproverActor
	for _, apiActor := range a.Approvers {
		actor := ApproverActor{
			PrincipalID: apiActor.PrincipalId,
		}
		if apiActor.DisplayName != nil {
			actor.DisplayName = *apiActor.DisplayName
		}
		if apiActor.Role != nil {
			actor.Role = *apiActor.Role
		}
		if apiActor.Status != nil {
			actor.Status = ActorStatus(*apiActor.Status)
		}
		actors = append(actors, actor)
	}

	approval := Approval{
		ApprovalID:  a.ApprovalId,
		IntentID:    a.IntentId,
		Status:      apiStatusToStatus(a.Status),
		Approvers:   actors,
		ExpiresAt:   a.ExpiresAt,
		RequestedAt: a.RequestedAt,
		ResolvedAt:  a.ResolvedAt,
	}
	if a.WorkspaceId != nil {
		approval.WorkspaceID = *a.WorkspaceId
	}
	if a.Rationale != nil {
		approval.Rationale = *a.Rationale
	}
	if a.EditableBounds != nil {
		approval.EditableBounds = *a.EditableBounds
	}
	return approval
}

func statusToAPIStatus(s Status) api.ApprovalStatus {
	return api.ApprovalStatus(s)
}

func apiStatusToStatus(s api.ApprovalStatus) Status {
	return Status(s)
}

func toMapPtr(m map[string]any) *map[string]interface{} {
	if m == nil {
		return nil
	}
	return &m
}
