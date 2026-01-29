package main

import (
	"context"
	"log"

	"github.com/bucknercd/jobworker/internal/manager"
	jobpb "github.com/bucknercd/jobworker/proto/gen/jobpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type grpcServer struct {
	jobpb.UnimplementedJobWorkerServer
	logger *log.Logger
	mgr    *manager.Manager
}

func NewGRPCServer(logger *log.Logger, mgr *manager.Manager) jobpb.JobWorkerServer {
	return &grpcServer{logger: logger, mgr: mgr}
}

func (s *grpcServer) StartJob(ctx context.Context, req *jobpb.StartJobRequest) (*jobpb.StartJobResponse, error) {
	user, err := mtlsUserFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "mTLS identity: %v", err)
	}

	// For now: log it. Next step: pass it to manager/joblib for authz/auditing.
	s.logger.Printf("StartJob user=%s exe=%q args=%v", user, req.GetExecutable(), req.GetArgs())

	return s.mgr.StartJob(ctx, req)
}

func (s *grpcServer) StopJob(ctx context.Context, req *jobpb.StopJobRequest) (*jobpb.StopJobResponse, error) {
	return s.mgr.StopJob(ctx, req)
}

func (s *grpcServer) GetStatus(ctx context.Context, req *jobpb.GetStatusRequest) (*jobpb.GetStatusResponse, error) {
	return s.mgr.GetStatus(ctx, req)
}

func (s *grpcServer) StreamOutput(req *jobpb.StreamOutputRequest, stream jobpb.JobWorker_StreamOutputServer) error {
	// Not implemented in manager yet; keep it explicit.
	// If you already have it in joblib, we can wire next.
	return jobpb.UnimplementedJobWorkerServer{}.StreamOutput(req, stream)
}
