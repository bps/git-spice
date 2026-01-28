package azuredevops

import (
	"context"
	"fmt"

	"github.com/microsoft/azure-devops-go-api/azuredevops/v7/git"
	"go.abhg.dev/gs/internal/forge"
)

// ChangesStates fetches the states of the given change IDs.
func (r *Repository) ChangesStates(
	ctx context.Context,
	ids []forge.ChangeID,
) ([]forge.ChangeState, error) {
	states := make([]forge.ChangeState, len(ids))

	// Azure DevOps doesn't have a batch API for fetching PR states,
	// so we need to fetch each PR individually.
	for i, id := range ids {
		prID := mustPR(id).Number

		pr, err := r.client.gitClient.GetPullRequest(ctx, git.GetPullRequestArgs{
			Project:       strPtr(r.project()),
			RepositoryId:  strPtr(r.repositoryID()),
			PullRequestId: &prID,
		})
		if err != nil {
			return nil, fmt.Errorf("get pull request %d: %w", prID, err)
		}

		if pr.Status != nil {
			states[i] = mapPRStatusToChangeState(*pr.Status)
		}
	}

	return states, nil
}

// mapPRStatusToChangeState maps an Azure DevOps PR status
// to a forge.ChangeState.
func mapPRStatusToChangeState(status git.PullRequestStatus) forge.ChangeState {
	switch status {
	case git.PullRequestStatusValues.Active:
		return forge.ChangeOpen
	case git.PullRequestStatusValues.Completed:
		return forge.ChangeMerged
	case git.PullRequestStatusValues.Abandoned:
		return forge.ChangeClosed
	default:
		// NotSet or unknown status - treat as open.
		return forge.ChangeOpen
	}
}
