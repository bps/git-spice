package spice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.abhg.dev/gs/internal/cli"
	"go.abhg.dev/gs/internal/git"
	"go.abhg.dev/gs/internal/must"
	"go.abhg.dev/gs/internal/spice/state"
)

// BranchOntoRequest is a request to move a branch onto another branch.
type BranchOntoRequest struct {
	// Branch is the branch to move.
	// This must not be the trunk branch.
	Branch string

	// Onto is the target branch to move the branch onto.
	// Onto may be the trunk branch.
	Onto string

	// MergedDownstack for [Branch], if any.
	MergedDownstack *[]json.RawMessage

	// SkipRebase indicates that the branch's base should be updated,
	// but no rebase should be performed.
	// The old base hash is preserved to allow future restack operations
	// to correctly rebase the branch.
	SkipRebase bool
}

// BranchOnto moves the commits of a branch onto a different base branch,
// updating internal state to reflect the new branch stack.
// It DOES NOT modify the upstack branches of the branch being moved.
// As this involves a rebase operation,
// the caller should be prepared to rescue the operation if it fails.
func (s *Service) BranchOnto(
	ctx context.Context, req *BranchOntoRequest,
) error {
	return s.branchOntoWith(ctx, req, s.wt)
}

// BranchOntoInWorktree moves a branch onto a new base
// by rebasing it in another worktree
// where the branch is checked out.
//
// If the worktree cannot be opened,
// or if a conflict occurs during the rebase,
// the operation falls back to updating state only
// (SkipRebase) and logs a warning.
// The branch will be left in a "needs restack" state
// that can be resolved with 'git-spice branch restack'.
func (s *Service) BranchOntoInWorktree(
	ctx context.Context,
	req *BranchOntoRequest,
	branchWT string,
) error {
	otherWT, err := s.repo.OpenWorktree(ctx, branchWT)
	if err != nil {
		s.log.Warn(
			"Could not open worktree,"+
				" skipping rebase",
			"branch", req.Branch,
			"worktree", branchWT,
			"error", err,
		)
		return s.branchOntoSkipRebase(ctx, req)
	}

	err = s.branchOntoWith(ctx, req, otherWT)
	if err != nil {
		var rebaseErr *git.RebaseInterruptError
		if errors.As(err, &rebaseErr) {
			// Conflict in the other worktree.
			// Abort to leave it clean.
			if abortErr := otherWT.RebaseAbort(
				ctx,
			); abortErr != nil {
				s.log.Warn(
					"Could not abort rebase"+
						" in worktree",
					"branch", req.Branch,
					"worktree", branchWT,
					"error", abortErr,
				)
			}
			s.log.Warn(
				"Conflict while moving branch"+
					" in worktree,"+
					" skipping rebase",
				"branch", req.Branch,
				"onto", req.Onto,
				"worktree", branchWT,
			)
			s.log.Warn(
				fmt.Sprintf(
					"Run '%s branch restack'"+
						" from the worktree"+
						" to resolve",
					cli.Name(),
				),
				"branch", req.Branch,
			)
		} else {
			s.log.Warn(
				"Could not move branch"+
					" in worktree,"+
					" skipping rebase",
				"branch", req.Branch,
				"onto", req.Onto,
				"worktree", branchWT,
				"error", err,
			)
		}
		return s.branchOntoSkipRebase(ctx, req)
	}

	s.log.Info(
		"Moved branch in worktree",
		"branch", req.Branch,
		"onto", req.Onto,
		"worktree", branchWT,
	)
	return nil
}

// branchOntoSkipRebase updates state for a branch onto
// without performing a rebase.
func (s *Service) branchOntoSkipRebase(
	ctx context.Context,
	req *BranchOntoRequest,
) error {
	skipReq := *req
	skipReq.SkipRebase = true
	return s.branchOntoWith(ctx, &skipReq, s.wt)
}

func (s *Service) branchOntoWith(
	ctx context.Context,
	req *BranchOntoRequest,
	wt GitWorktree,
) error {
	must.NotBeEqualf(
		req.Branch, s.store.Trunk(), "cannot move trunk",
	)

	branch, err := s.LookupBranch(ctx, req.Branch)
	if err != nil {
		return fmt.Errorf("lookup branch: %w", err)
	}

	var ontoHash git.Hash
	if req.Onto == s.store.Trunk() {
		ontoHash, err = s.repo.PeelToCommit(ctx, req.Onto)
		if err != nil {
			return fmt.Errorf("resolve trunk: %w", err)
		}
	} else {
		// Non-trunk branches must be tracked.
		onto, err := s.LookupBranch(ctx, req.Onto)
		if err != nil {
			return fmt.Errorf("lookup onto: %w", err)
		}
		ontoHash = onto.Head
	}

	// We're trying to move commits
	// BaseHash..HEAD onto commit OntoHash.
	//
	// However, there's a possibility that
	// BaseHash is reachable from OntoHash
	// because the old base is also the base of onto,
	// and we've already partially rebased
	// and handled a conflict.
	//
	// For example, suppose we have:
	//
	//           C--D (Current)  (git-spice: base=OriginalBase)
	//          /
	//     o---X (OriginalBase)
	//          \
	//           A--B (NewBase)  (git-spice: base=OriginalBase)
	//
	// If we run 'git-spice branch onto NewBase' from Current,
	// and there's a conflict, the user will resolve the rebase conflict,
	// but the git-spice state will not yet be updated.
	//
	//     o---X (OriginalBase)
	//          \
	//           A--B (NewBase)       (git-spice: base=OriginalBase)
	//               \
	//                C--D (Current)  (git-spice: base=OriginalBase)
	//
	// At that point, 'git-spice rebase continue' will re-run the original command
	// 'git-spice branch onto NewBase' from Current,
	// except the commits it wants (OriginalBase..Current)
	// now includes commits OriginalBase..NewBase,
	// which will fail for obvious reasons.
	//
	// To catch this, if OriginalBase is reachable
	// from NewBase, we'll change the commit range
	// to NewBase..Current.
	// This will turn the rebase into a no-op,
	// but it'll correctly update state.
	fromHash := branch.BaseHash
	if s.repo.IsAncestor(ctx, fromHash, ontoHash) {
		fromHash = ontoHash
	}

	s.log.Debug("Moving commits onto new base",
		"branch", req.Branch,
		"oldBase", branch.Base,
		"newBase", req.Onto,
		"commits",
		fromHash.Short()+".."+branch.Head.Short(),
	)

	branchTx := s.store.BeginBranchTx()

	// When SkipRebase is true, we update the base branch name
	// but preserve the old base hash.
	// This leaves the branch in a "needs restack" state
	// that can be detected and corrected later.
	baseHash := ontoHash
	if req.SkipRebase {
		baseHash = branch.BaseHash
	}

	if err := branchTx.Upsert(ctx, state.UpsertRequest{
		Name:            req.Branch,
		Base:            req.Onto,
		BaseHash:        baseHash,
		MergedDownstack: req.MergedDownstack,
	}); err != nil {
		return fmt.Errorf(
			"set base of branch %s to %s: %w",
			req.Branch, req.Onto, err,
		)
	}

	if !req.SkipRebase {
		if err := wt.Rebase(ctx, git.RebaseRequest{
			Branch:    req.Branch,
			Upstream:  string(fromHash),
			Onto:      ontoHash.String(),
			Autostash: true,
			Quiet:     true,
		}); err != nil {
			return fmt.Errorf("rebase: %w", err)
		}
	}

	if err := branchTx.Commit(ctx,
		fmt.Sprintf("%v: onto %v", req.Branch, req.Onto),
	); err != nil {
		return fmt.Errorf("update state: %w", err)
	}

	return nil
}
