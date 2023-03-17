package service

import (
	schemaV1 "buf.build/gen/go/open-feature/flagd/protocolbuffers/go/schema/v1"
	"context"
	"fmt"
	"github.com/bufbuild/connect-go"
	"github.com/open-feature/flagd/core/pkg/eval"
	"github.com/open-feature/flagd/core/pkg/logger"
	"github.com/open-feature/flagd/core/pkg/model"
	"github.com/open-feature/flagd/core/pkg/otel"
	"github.com/rs/xid"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"
	"sync"
	"time"
)

type FlagEvaluationService struct {
	logger                *logger.Logger
	eval                  eval.IEvaluator
	eventingConfiguration eventingConfiguration
}

type eventingConfiguration struct {
	mu   *sync.RWMutex
	subs map[interface{}]chan Notification
}

func (s *FlagEvaluationService) ResolveAll(
	ctx context.Context,
	req *connect.Request[schemaV1.ResolveAllRequest],
) (*connect.Response[schemaV1.ResolveAllResponse], error) {
	reqID := xid.New().String()
	defer s.logger.ClearFields(reqID)
	res := &schemaV1.ResolveAllResponse{
		Flags: make(map[string]*schemaV1.AnyFlag),
	}
	values := s.eval.ResolveAllValues(reqID, req.Msg.GetContext())
	for _, value := range values {
		switch v := value.Value.(type) {
		case bool:
			res.Flags[value.FlagKey] = &schemaV1.AnyFlag{
				Reason:  value.Reason,
				Variant: value.Variant,
				Value: &schemaV1.AnyFlag_BoolValue{
					BoolValue: v,
				},
			}
		case string:
			res.Flags[value.FlagKey] = &schemaV1.AnyFlag{
				Reason:  value.Reason,
				Variant: value.Variant,
				Value: &schemaV1.AnyFlag_StringValue{
					StringValue: v,
				},
			}
		case float64:
			res.Flags[value.FlagKey] = &schemaV1.AnyFlag{
				Reason:  value.Reason,
				Variant: value.Variant,
				Value: &schemaV1.AnyFlag_DoubleValue{
					DoubleValue: v,
				},
			}
		case map[string]any:
			val, err := structpb.NewStruct(v)
			if err != nil {
				s.logger.ErrorWithID(reqID, fmt.Sprintf("struct response construction: %v", err))
				continue
			}
			res.Flags[value.FlagKey] = &schemaV1.AnyFlag{
				Reason:  value.Reason,
				Variant: value.Variant,
				Value: &schemaV1.AnyFlag_ObjectValue{
					ObjectValue: val,
				},
			}
		}
	}
	return connect.NewResponse(res), nil
}

func (s *FlagEvaluationService) EventStream(
	ctx context.Context,
	req *connect.Request[schemaV1.EventStreamRequest],
	stream *connect.ServerStream[schemaV1.EventStreamResponse],
) error {
	requestNotificationChan := make(chan Notification, 1)
	s.eventingConfiguration.mu.Lock()
	s.eventingConfiguration.subs[req] = requestNotificationChan
	s.eventingConfiguration.mu.Unlock()
	defer func() {
		s.eventingConfiguration.mu.Lock()
		delete(s.eventingConfiguration.subs, req)
		s.eventingConfiguration.mu.Unlock()
	}()
	requestNotificationChan <- Notification{
		Type: ProviderReady,
	}
	for {
		select {
		case <-time.After(20 * time.Second):
			err := stream.Send(&schemaV1.EventStreamResponse{
				Type: string(KeepAlive),
			})
			if err != nil {
				s.logger.Error(err.Error())
			}
		case notification := <-requestNotificationChan:
			d, err := structpb.NewStruct(notification.Data)
			if err != nil {
				s.logger.Error(err.Error())
			}
			err = stream.Send(&schemaV1.EventStreamResponse{
				Type: string(notification.Type),
				Data: d,
			})
			if err != nil {
				s.logger.Error(err.Error())
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func resolve[T constraints](
	logger *logger.Logger,
	resolver func(reqID, flagKey string, ctx *structpb.Struct) (T, string, string, error),
	flagKey string,
	ctx *structpb.Struct,
	resp response[T],
	goCtx context.Context,
	metrics otel.MetricsRecorder,
) error {
	reqID := xid.New().String()
	defer logger.ClearFields(reqID)

	logger.WriteFields(
		reqID,
		zap.String("flag-key", flagKey),
		zap.Strings("context-keys", formatContextKeys(ctx)),
	)

	result, variant, reason, evalErr := resolver(reqID, flagKey, ctx)
	if evalErr != nil {
		logger.WarnWithID(reqID, fmt.Sprintf("returning error response, reason: %v", evalErr))
		reason = model.ErrorReason
		evalErr = errFormat(evalErr)
	}
	defer func() {
		metrics.OTelImpressions(goCtx, flagKey, variant)
	}()

	if err := resp.SetResult(result, variant, reason); err != nil && evalErr == nil {
		logger.ErrorWithID(reqID, err.Error())
		return err
	}

	return evalErr
}

func (s *ConnectService) ResolveBoolean(
	ctx context.Context,
	req *connect.Request[schemaV1.ResolveBooleanRequest],
) (*connect.Response[schemaV1.ResolveBooleanResponse], error) {
	res := connect.NewResponse(&schemaV1.ResolveBooleanResponse{})
	err := resolve[bool](
		s.Logger, s.Eval.ResolveBooleanValue, req.Msg.GetFlagKey(), req.Msg.GetContext(), &booleanResponse{res}, ctx, s.metrics,
	)

	return res, err
}

func (s *ConnectService) ResolveString(
	ctx context.Context,
	req *connect.Request[schemaV1.ResolveStringRequest],
) (*connect.Response[schemaV1.ResolveStringResponse], error) {
	res := connect.NewResponse(&schemaV1.ResolveStringResponse{})
	err := resolve[string](
		s.Logger, s.Eval.ResolveStringValue, req.Msg.GetFlagKey(), req.Msg.GetContext(), &stringResponse{res}, ctx, s.metrics,
	)

	return res, err
}

func (s *ConnectService) ResolveInt(
	ctx context.Context,
	req *connect.Request[schemaV1.ResolveIntRequest],
) (*connect.Response[schemaV1.ResolveIntResponse], error) {
	res := connect.NewResponse(&schemaV1.ResolveIntResponse{})
	err := resolve[int64](
		s.Logger, s.Eval.ResolveIntValue, req.Msg.GetFlagKey(), req.Msg.GetContext(), &intResponse{res}, ctx, s.metrics,
	)

	return res, err
}

func (s *ConnectService) ResolveFloat(
	ctx context.Context,
	req *connect.Request[schemaV1.ResolveFloatRequest],
) (*connect.Response[schemaV1.ResolveFloatResponse], error) {
	res := connect.NewResponse(&schemaV1.ResolveFloatResponse{})
	err := resolve[float64](
		s.Logger, s.Eval.ResolveFloatValue, req.Msg.GetFlagKey(), req.Msg.GetContext(), &floatResponse{res}, ctx, s.metrics,
	)

	return res, err
}

func (s *ConnectService) ResolveObject(
	ctx context.Context,
	req *connect.Request[schemaV1.ResolveObjectRequest],
) (*connect.Response[schemaV1.ResolveObjectResponse], error) {
	res := connect.NewResponse(&schemaV1.ResolveObjectResponse{})
	err := resolve[map[string]any](
		s.Logger, s.Eval.ResolveObjectValue, req.Msg.GetFlagKey(), req.Msg.GetContext(), &objectResponse{res}, ctx, s.metrics,
	)

	return res, err
}

func formatContextKeys(context *structpb.Struct) []string {
	res := []string{}
	for k := range context.AsMap() {
		res = append(res, k)
	}
	return res
}

func errFormat(err error) error {
	switch err.Error() {
	case model.FlagNotFoundErrorCode:
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s, %s", ErrorPrefix, err.Error()))
	case model.TypeMismatchErrorCode:
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s, %s", ErrorPrefix, err.Error()))
	case model.DisabledReason:
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s, %s", ErrorPrefix, err.Error()))
	case model.ParseErrorCode:
		return connect.NewError(connect.CodeDataLoss, fmt.Errorf("%s, %s", ErrorPrefix, err.Error()))
	}

	return err
}
