package plan

import (
	"fmt"
	"log"
	"plandex-server/db"
	"plandex-server/model"
	"plandex-server/types"
	"time"

	shared "plandex-shared"
)

func Build(
	clients map[string]model.ClientInfo,
	plan *db.Plan,
	branch string,
	auth *types.ServerAuth,
) (int, error) {
	log.Printf("Build: Called with plan ID %s on branch %s\n", plan.Id, branch)
	log.Println("Build: Starting Build operation")

	state := activeBuildStreamState{
		clients:       clients,
		auth:          auth,
		currentOrgId:  auth.OrgId,
		currentUserId: auth.User.Id,
		plan:          plan,
		branch:        branch,
	}

	streamDone := func() {
		active := GetActivePlan(plan.Id, branch)
		if active != nil {
			active.StreamDoneCh <- nil
		}
	}

	onErr := func(err error) (int, error) {
		log.Printf("Build error: %v\n", err)
		streamDone()
		return 0, err
	}

	pendingBuildsByPath, err := state.loadPendingBuilds()
	if err != nil {
		return onErr(err)
	}

	if len(pendingBuildsByPath) == 0 {
		log.Println("No pending builds")
		streamDone()
		return 0, nil
	}

	err = db.SetPlanStatus(plan.Id, branch, shared.PlanStatusBuilding, "")

	if err != nil {
		log.Printf("Error setting plan status to building: %v\n", err)
		return onErr(fmt.Errorf("error setting plan status to building: %v", err))
	}

	log.Printf("Starting %d builds\n", len(pendingBuildsByPath))

	for _, pendingBuilds := range pendingBuildsByPath {
		go state.queueBuilds(pendingBuilds)
	}

	return len(pendingBuildsByPath), nil
}

func (state *activeBuildStreamState) queueBuild(activeBuild *types.ActiveBuild) {
	planId := state.plan.Id
	branch := state.branch

	filePath := activeBuild.Path

	// log.Printf("Queue:")
	// spew.Dump(activePlan.BuildQueuesByPath[filePath])

	var isBuilding bool

	UpdateActivePlan(planId, branch, func(active *types.ActivePlan) {
		active.BuildQueuesByPath[filePath] = append(active.BuildQueuesByPath[filePath], activeBuild)
		isBuilding = active.IsBuildingByPath[filePath]
	})
	log.Printf("Queued build for file %s\n", filePath)

	if isBuilding {
		log.Printf("Already building file %s\n", filePath)
		return
	} else {
		log.Printf("Not building file %s\n", filePath)

		active := GetActivePlan(planId, branch)
		if active == nil {
			log.Printf("Active plan not found for plan ID %s and branch %s\n", planId, branch)
			return
		}

		UpdateActivePlan(planId, branch, func(active *types.ActivePlan) {
			active.IsBuildingByPath[filePath] = true
		})

		go state.execPlanBuild(activeBuild)
	}
}

func (state *activeBuildStreamState) queueBuilds(activeBuilds []*types.ActiveBuild) {
	log.Printf("Queueing %d builds\n", len(activeBuilds))

	for _, activeBuild := range activeBuilds {
		state.queueBuild(activeBuild)
	}
}

func (buildState *activeBuildStreamState) execPlanBuild(activeBuild *types.ActiveBuild) {
	if activeBuild == nil {
		log.Println("No active build")
		return
	}

	log.Printf("execPlanBuild - %s\n", activeBuild.Path)
	// log.Println(spew.Sdump(activeBuild))

	planId := buildState.plan.Id
	branch := buildState.branch

	activePlan := GetActivePlan(planId, branch)
	if activePlan == nil {
		log.Printf("Active plan not found for plan ID %s and branch %s\n", planId, branch)
		return
	}
	filePath := activeBuild.Path

	if !activePlan.IsBuildingByPath[filePath] {
		UpdateActivePlan(activePlan.Id, activePlan.Branch, func(ap *types.ActivePlan) {
			ap.IsBuildingByPath[filePath] = true
		})
	}

	fileState := &activeBuildStreamFileState{
		activeBuildStreamState: buildState,
		filePath:               filePath,
		activeBuild:            activeBuild,
	}

	log.Printf("execPlanBuild - %s - calling fileState.loadBuildFile()\n", filePath)
	err := fileState.loadBuildFile(activeBuild)
	if err != nil {
		log.Printf("Error loading build file: %v\n", err)
		fileState.onBuildFileError(fmt.Errorf("error loading build file: %v", err))
		return
	}

	fileState.resolvePreBuildState()

	// unless it's a file operation, stream initial status to client
	if !activeBuild.IsFileOperation() && !fileState.isNewFile {
		log.Printf("execPlanBuild - %s - streaming initial build info\n", filePath)
		// spew.Dump(activeBuild)
		buildInfo := &shared.BuildInfo{
			Path:      filePath,
			NumTokens: 0,
			Finished:  false,
		}
		activePlan.Stream(shared.StreamMessage{
			Type:      shared.StreamMessageBuildInfo,
			BuildInfo: buildInfo,
		})
	} else if activeBuild.IsFileOperation() {
		log.Printf("execPlanBuild - %s - file operation - won't stream initial build info\n", filePath)
	} else if fileState.isNewFile {
		log.Printf("execPlanBuild - %s - new file - won't stream initial build info\n", filePath)
	}

	log.Printf("execPlanBuild - %s - calling fileState.buildFile()\n", filePath)
	fileState.buildFile()
}

func (fileState *activeBuildStreamFileState) buildFile() {
	filePath := fileState.filePath
	activeBuild := fileState.activeBuild
	planId := fileState.plan.Id
	branch := fileState.branch
	currentOrgId := fileState.currentOrgId
	build := fileState.build

	activePlan := GetActivePlan(planId, branch)

	if activePlan == nil {
		log.Printf("Active plan not found for plan ID %s and branch %s\n", planId, branch)
		return
	}

	log.Printf("Building file %s\n", filePath)

	log.Printf("%d files in context\n", len(activePlan.ContextsByPath))

	// log.Println("activePlan.ContextsByPath files:")
	// for k := range activePlan.ContextsByPath {
	// 	log.Println(k)
	// }

	if activeBuild.IsMoveOp {
		log.Printf("File %s is a move operation. Moving to %s\n", filePath, activeBuild.MoveDestination)

		// For move operations, we split it into two separate builds:
		// 1. A removal build for the source file
		// 2. A creation build for the destination file with the current content
		// This is simpler than handling moves in a single build since our build system
		// is designed around operating on one path at a time
		fileState.activeBuildStreamState.queueBuilds([]*types.ActiveBuild{
			{
				ReplyId:    activeBuild.ReplyId,
				Path:       activeBuild.Path,
				IsRemoveOp: true,
			},
			{
				ReplyId:           activeBuild.ReplyId,
				Path:              activeBuild.MoveDestination,
				FileContent:       fileState.preBuildState,
				FileContentTokens: 0,
			},
		})

		// Mark this move operation as successful since we've queued the actual work
		activeBuild.Success = true

		UpdateActivePlan(planId, branch, func(active *types.ActivePlan) {
			active.IsBuildingByPath[filePath] = false
			active.BuiltFiles[filePath] = true
		})

		// Process the next build in queue (which will be our removal build)
		// We need to explicitly advance the queue for the source path since this
		// current build is holding the 'building' state open
		// The create build for the destination will be handled automatically by the queue logic
		fileState.buildNextInQueue()
		return
	}

	if activeBuild.IsRemoveOp {
		log.Printf("File %s is a remove operation. Removing file.\n", filePath)

		log.Printf("streaming remove build info for file %s\n", filePath)
		buildInfo := &shared.BuildInfo{
			Path:      filePath,
			NumTokens: 0,
			Removed:   true,
			Finished:  true,
		}

		activePlan.Stream(shared.StreamMessage{
			Type:      shared.StreamMessageBuildInfo,
			BuildInfo: buildInfo,
		})

		planRes := &db.PlanFileResult{
			OrgId:          currentOrgId,
			PlanId:         planId,
			PlanBuildId:    build.Id,
			ConvoMessageId: build.ConvoMessageId,
			Path:           filePath,
			Content:        "",
			RemovedFile:    true,
		}
		fileState.onFinishBuildFile(planRes)
		return
	}

	if activeBuild.IsResetOp {
		log.Printf("File %s is a reset operation. Resetting file.\n", filePath)

		err := activePlan.LockForActiveBuild(db.LockScopeWrite, build.Id)
		if err != nil {
			log.Printf("Error locking active plan for reset: %v\n", err)
			fileState.onBuildFileError(fmt.Errorf("error locking active plan for reset: %v", err))
			return
		}

		now := time.Now()
		err = db.RejectPlanFile(currentOrgId, planId, filePath, now)
		if err != nil {
			log.Printf("Error rejecting plan file: %v\n", err)
			unlockErr := activePlan.UnlockForActiveBuild()
			if unlockErr != nil {
				log.Printf("Error unlocking active plan for reset: %v\n", unlockErr)
			}
			fileState.onBuildFileError(fmt.Errorf("error rejecting plan file: %v", err))
			return
		}
		err = activePlan.UnlockForActiveBuild()
		if err != nil {
			log.Printf("Error unlocking active plan for reset: %v\n", err)
			fileState.onBuildFileError(fmt.Errorf("error unlocking active plan for reset: %v", err))
			return
		}

		buildInfo := &shared.BuildInfo{
			Path:      filePath,
			NumTokens: 0,
			Finished:  true,
			Removed:   fileState.contextPart == nil,
		}

		activePlan.Stream(shared.StreamMessage{
			Type:      shared.StreamMessageBuildInfo,
			BuildInfo: buildInfo,
		})

		time.Sleep(200 * time.Millisecond)

		fileState.onBuildProcessed(activeBuild)
		return
	}

	if fileState.preBuildState == "" {
		log.Printf("File %s not found in model context or current plan. Creating new file.\n", filePath)

		buildInfo := &shared.BuildInfo{
			Path:      filePath,
			NumTokens: 0,
			Finished:  true,
		}

		log.Printf("streaming new file build info for file %s\n", filePath)

		activePlan.Stream(shared.StreamMessage{
			Type:      shared.StreamMessageBuildInfo,
			BuildInfo: buildInfo,
		})

		// new file
		planRes := &db.PlanFileResult{
			OrgId:          currentOrgId,
			PlanId:         planId,
			PlanBuildId:    build.Id,
			ConvoMessageId: build.ConvoMessageId,
			Path:           filePath,
			Content:        activeBuild.FileContent,
		}

		// log.Println("build exec - new file result")
		// spew.Dump(planRes)
		fileState.onFinishBuildFile(planRes)
		return
	} else {
		currentNumTokens := shared.GetNumTokensEstimate(fileState.preBuildState)

		log.Printf("Current state num tokens: %d\n", currentNumTokens)

		activeBuild.CurrentFileTokens = currentNumTokens
		activePlan.DidEditFiles = true
	}

	// build structured edits strategy now works regardless of language/tree-sitter support
	log.Println("buildFile - building structured edits")
	fileState.buildStructuredEdits()
}

func (fileState *activeBuildStreamFileState) resolvePreBuildState() {
	filePath := fileState.filePath
	currentPlan := fileState.currentPlanState
	planId := fileState.plan.Id
	branch := fileState.branch

	activePlan := GetActivePlan(planId, branch)

	if activePlan == nil {
		log.Printf("Active plan not found for plan ID %s and branch %s\n", planId, branch)
		return
	}
	contextPart := activePlan.ContextsByPath[filePath]

	var currentState string
	currentPlanFile, fileInCurrentPlan := currentPlan.CurrentPlanFiles.Files[filePath]

	// log.Println("plan files:")
	// spew.Dump(currentPlan.CurrentPlanFiles.Files)

	if fileInCurrentPlan {
		log.Printf("File %s found in current plan.\n", filePath)
		fileState.isNewFile = false
		currentState = currentPlanFile
		// log.Println("\n\nCurrent state:\n", currentState, "\n\n")

	} else if contextPart != nil {
		log.Printf("File %s found in model context. Using context state.\n", filePath)
		fileState.isNewFile = false
		currentState = contextPart.Body
		// log.Println("\n\nCurrent state:\n", currentState, "\n\n")
	} else {
		fileState.isNewFile = true
	}

	fileState.preBuildState = currentState
	fileState.contextPart = contextPart
}
