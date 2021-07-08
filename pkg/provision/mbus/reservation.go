package mbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/threefoldtech/zos/pkg/gridtypes"
	"github.com/threefoldtech/zos/pkg/provision"
	"github.com/threefoldtech/zos/pkg/provision/mw"
	"github.com/threefoldtech/zos/pkg/rmb"
)

type idArgs struct {
	DeploymentID uint32 `json:"deployment_id"`
}

func (d *Deployments) createOrUpdate(ctx context.Context, payload []byte, update bool) (interface{}, mw.Response) {
	var deployment gridtypes.Deployment
	if err := json.Unmarshal(payload, &deployment); err != nil {
		return nil, mw.BadRequest(err)
	}

	if err := deployment.Valid(); err != nil {
		return nil, mw.BadRequest(err)
	}

	deployment.TwinID = rmb.GetTwinID(ctx)
	if err := deployment.Verify(d.engine.Twins()); err != nil {
		return nil, mw.UnAuthorized(err)
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	action := d.engine.Provision
	if update {
		action = d.engine.Update
	}

	err := action(ctx, deployment)

	if err == context.DeadlineExceeded {
		return nil, mw.Unavailable(ctx.Err())
	} else if errors.Is(err, provision.ErrDeploymentExists) {
		return nil, mw.Conflict(err)
	} else if errors.Is(err, provision.ErrDeploymentNotExists) {
		return nil, mw.NotFound(err)
	} else if errors.Is(err, provision.ErrDeploymentUpgradeValidationError) {
		return nil, mw.BadRequest(err)
	} else if err != nil {
		return nil, mw.Error(err)
	}

	return nil, mw.Accepted()
}

func (d *Deployments) delete(ctx context.Context, payload []byte) (interface{}, mw.Response) {
	var args idArgs
	err := json.Unmarshal(payload, &args)
	if err != nil {
		return nil, mw.Error(err)
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	err = d.engine.Deprovision(ctx, rmb.GetTwinID(ctx), args.DeploymentID, "requested by user")
	if err == context.DeadlineExceeded {
		return nil, mw.Unavailable(ctx.Err())
	} else if errors.Is(err, provision.ErrDeploymentNotExists) {
		return nil, mw.NotFound(err)
	} else if err != nil {
		return nil, mw.Error(err)
	}

	return nil, mw.Accepted()
}

func (d *Deployments) get(ctx context.Context, payload []byte) (interface{}, mw.Response) {
	var args idArgs
	err := json.Unmarshal(payload, &args)
	if err != nil {
		return nil, mw.Error(err)
	}

	deployment, err := d.engine.Storage().Get(rmb.GetTwinID(ctx), args.DeploymentID)
	if errors.Is(err, provision.ErrDeploymentNotExists) {
		return nil, mw.NotFound(fmt.Errorf("workload not found"))
	} else if err != nil {
		return nil, mw.Error(err)
	}

	return deployment, nil
}