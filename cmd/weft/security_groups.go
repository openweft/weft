package main

// security_groups.go implements the six SecurityGroup RPCs.
// SetSecurityGroupRules replaces the rule list atomically, in
// line with the registry's semantics (a single Save covers the
// whole new state — no partial mutation, no per-rule patch).

import (
	"context"

	"github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func ruleToProto(r weft.SecurityRule) *weftv1.SecurityRule {
	return &weftv1.SecurityRule{
		Direction:       string(r.Direction),
		Protocol:        string(r.Protocol),
		PortMin:         int32(r.PortMin),
		PortMax:         int32(r.PortMax),
		RemoteCidr:      r.RemoteCIDR,
		RemoteGroupUuid: r.RemoteGroup,
	}
}

func ruleFromProto(p *weftv1.SecurityRule) weft.SecurityRule {
	return weft.SecurityRule{
		Direction:   weft.SecurityRuleDirection(p.Direction),
		Protocol:    weft.SecurityRuleProtocol(p.Protocol),
		PortMin:     int(p.PortMin),
		PortMax:     int(p.PortMax),
		RemoteCIDR:  p.RemoteCidr,
		RemoteGroup: p.RemoteGroupUuid,
	}
}

func toSecurityGroupInfo(g weft.SecurityGroup) *weftv1.SecurityGroupInfo {
	rules := make([]*weftv1.SecurityRule, len(g.Rules))
	for i, r := range g.Rules {
		rules[i] = ruleToProto(r)
	}
	return &weftv1.SecurityGroupInfo{
		Uuid:            g.UUID,
		ProjectUuid:     g.ProjectUUID,
		Name:            g.Name,
		Description:     g.Description,
		Rules:           rules,
		CreatedAtUnixNs: g.CreatedAt.UnixNano(),
	}
}

func (s *weftServer) ListSecurityGroups(ctx context.Context, req *weftv1.ListSecurityGroupsRequest) (*weftv1.ListSecurityGroupsResponse, error) {
	visible, all, err := s.adp.VisibleProjects(ctx)
	if err != nil {
		return nil, err
	}
	var wantProjectUUID string
	if req.Project != "" {
		uuid, err := s.adp.AuthorizeProject(ctx, req.Project)
		if err != nil {
			return nil, err
		}
		wantProjectUUID = uuid
	}
	out := []*weftv1.SecurityGroupInfo{}
	for _, g := range s.adp.SecurityGroups() {
		if wantProjectUUID != "" && g.ProjectUUID != wantProjectUUID {
			continue
		}
		if !all {
			if _, ok := visible[g.ProjectUUID]; !ok {
				continue
			}
		}
		out = append(out, toSecurityGroupInfo(g))
	}
	return &weftv1.ListSecurityGroupsResponse{Groups: out}, nil
}

func (s *weftServer) CreateSecurityGroup(ctx context.Context, req *weftv1.CreateSecurityGroupRequest) (*weftv1.CreateSecurityGroupResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	rules := make([]weft.SecurityRule, len(req.Rules))
	for i, r := range req.Rules {
		rules[i] = ruleFromProto(r)
	}
	g, err := s.adp.CreateSecurityGroup(weft.CreateSecurityGroupSpec{
		ProjectUUID: projUUID,
		Name:        req.Name,
		Description: req.Description,
		Rules:       rules,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create security group: %v", err)
	}
	logger.Printf("CreateSecurityGroup name=%s project=%s uuid=%s rules=%d", g.Name, g.ProjectUUID, g.UUID, len(g.Rules))
	return &weftv1.CreateSecurityGroupResponse{Group: toSecurityGroupInfo(g)}, nil
}

func (s *weftServer) authSecurityGroup(ctx context.Context, uuid string) (weft.SecurityGroup, error) {
	if uuid == "" {
		return weft.SecurityGroup{}, status.Error(codes.InvalidArgument, "uuid is required")
	}
	g, ok := s.adp.SecurityGroupByUUID(uuid)
	if !ok {
		return weft.SecurityGroup{}, status.Errorf(codes.PermissionDenied, "no access to security group %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, g.ProjectUUID); err != nil {
		return weft.SecurityGroup{}, err
	}
	return g, nil
}

func (s *weftServer) RenameSecurityGroup(ctx context.Context, req *weftv1.RenameSecurityGroupRequest) (*weftv1.RenameSecurityGroupResponse, error) {
	if req.NewName == "" {
		return nil, status.Error(codes.InvalidArgument, "new_name is required")
	}
	if _, err := s.authSecurityGroup(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.RenameSecurityGroup(req.Uuid, req.NewName); err != nil {
		return nil, status.Errorf(codes.Internal, "rename security group: %v", err)
	}
	g, _ := s.adp.SecurityGroupByUUID(req.Uuid)
	return &weftv1.RenameSecurityGroupResponse{Group: toSecurityGroupInfo(g)}, nil
}

func (s *weftServer) SetSecurityGroupDescription(ctx context.Context, req *weftv1.SetSecurityGroupDescriptionRequest) (*weftv1.SetSecurityGroupDescriptionResponse, error) {
	if _, err := s.authSecurityGroup(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.SetSecurityGroupDescription(req.Uuid, req.Description); err != nil {
		return nil, status.Errorf(codes.Internal, "set description: %v", err)
	}
	g, _ := s.adp.SecurityGroupByUUID(req.Uuid)
	return &weftv1.SetSecurityGroupDescriptionResponse{Group: toSecurityGroupInfo(g)}, nil
}

func (s *weftServer) SetSecurityGroupRules(ctx context.Context, req *weftv1.SetSecurityGroupRulesRequest) (*weftv1.SetSecurityGroupRulesResponse, error) {
	if _, err := s.authSecurityGroup(ctx, req.Uuid); err != nil {
		return nil, err
	}
	rules := make([]weft.SecurityRule, len(req.Rules))
	for i, r := range req.Rules {
		rules[i] = ruleFromProto(r)
	}
	if err := s.adp.SetSecurityGroupRules(req.Uuid, rules); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set rules: %v", err)
	}
	g, _ := s.adp.SecurityGroupByUUID(req.Uuid)
	return &weftv1.SetSecurityGroupRulesResponse{Group: toSecurityGroupInfo(g)}, nil
}

func (s *weftServer) DeleteSecurityGroup(ctx context.Context, req *weftv1.DeleteSecurityGroupRequest) (*weftv1.DeleteSecurityGroupResponse, error) {
	if _, err := s.authSecurityGroup(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteSecurityGroup(req.Uuid); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "delete security group: %v", err)
	}
	logger.Printf("DeleteSecurityGroup uuid=%s", req.Uuid)
	return &weftv1.DeleteSecurityGroupResponse{}, nil
}
