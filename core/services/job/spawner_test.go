package job_test

import (
	"context"
	"testing"
	"time"

	"github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/store/models"

	"gorm.io/gorm"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/job/mocks"
	"github.com/smartcontractkit/chainlink/core/services/offchainreporting"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	"github.com/smartcontractkit/chainlink/core/services/postgres"
	"gopkg.in/guregu/null.v4"
)

type delegate struct {
	jobType                    job.Type
	services                   []job.Service
	jobID                      int32
	chContinueCreatingServices chan struct{}
	job.Delegate
}

func (d delegate) JobType() job.Type {
	return d.jobType
}

func (d delegate) ServicesForSpec(js job.SpecDB) ([]job.Service, error) {
	if js.Type != d.jobType {
		return nil, nil
	}
	return d.services, nil
}

func clearDB(t *testing.T, db *gorm.DB) {
	err := db.Exec(`TRUNCATE jobs, pipeline_runs, pipeline_specs, pipeline_task_runs, pipeline_task_specs CASCADE`).Error
	require.NoError(t, err)
}

func TestSpawner_CreateJobDeleteJob(t *testing.T) {
	jobTypeA := job.DirectRequest
	jobTypeB := job.OffchainReporting

	config, oldORM, cleanupDB := cltest.BootstrapThrowawayORM(t, "services_job_spawner", true, true)
	defer cleanupDB()
	db := oldORM.DB

	eventBroadcaster := postgres.NewEventBroadcaster(config.DatabaseURL(), 0, 0)
	eventBroadcaster.Start()
	defer eventBroadcaster.Stop()

	key := cltest.MustInsertRandomKey(t, db)
	address := key.Address.Address()

	rpc, geth, _, _ := cltest.NewEthMocks(t)
	rpc.On("CallContext", mock.Anything, mock.Anything, "eth_getBlockByNumber", mock.Anything, false).
		Run(func(args mock.Arguments) {
			head := args.Get(1).(**models.Head)
			*head = cltest.Head(10)
		}).
		Return(nil)

	t.Run("starts and stops job services when jobs are added and removed", func(t *testing.T) {
		jobSpecA := makeOCRJobSpec(t, address)
		jobSpecA.Type = jobTypeA
		jobSpecB := makeOCRJobSpec(t, address)
		jobSpecB.Type = jobTypeB

		orm := job.NewORM(db, config.Config, pipeline.NewORM(db, config, eventBroadcaster), eventBroadcaster, &postgres.NullAdvisoryLocker{})
		defer orm.Close()
		eventuallyA := cltest.NewAwaiter()
		serviceA1 := new(mocks.Service)
		serviceA2 := new(mocks.Service)
		serviceA1.On("Start").Return(nil).Once()
		serviceA2.On("Start").Return(nil).Once().Run(func(mock.Arguments) { eventuallyA.ItHappened() })
		delegateA := &delegate{jobTypeA, []job.Service{serviceA1, serviceA2}, 0, make(chan struct{}), offchainreporting.NewDelegate(nil, orm, nil, nil, nil, eth.NewClientWith(rpc, geth), nil, nil, monitoringEndpoint)}
		eventuallyB := cltest.NewAwaiter()
		serviceB1 := new(mocks.Service)
		serviceB2 := new(mocks.Service)
		serviceB1.On("Start").Return(nil).Once()
		serviceB2.On("Start").Return(nil).Once().Run(func(mock.Arguments) { eventuallyB.ItHappened() })

		delegateB := &delegate{jobTypeB, []job.Service{serviceB1, serviceB2}, 0, make(chan struct{}), offchainreporting.NewDelegate(nil, orm, nil, nil, nil, eth.NewClientWith(rpc, geth), nil, nil, monitoringEndpoint)}
		spawner := job.NewSpawner(orm, config, map[job.Type]job.Delegate{
			jobTypeA: delegateA,
			jobTypeB: delegateB,
		})
		spawner.Start()
		jobSpecIDA, err := spawner.CreateJob(context.Background(), *jobSpecA, null.String{})
		require.NoError(t, err)
		delegateA.jobID = jobSpecIDA
		close(delegateA.chContinueCreatingServices)

		eventuallyA.AwaitOrFail(t, 20*time.Second)
		mock.AssertExpectationsForObjects(t, serviceA1, serviceA2)

		jobSpecIDB, err := spawner.CreateJob(context.Background(), *jobSpecB, null.String{})
		require.NoError(t, err)
		delegateB.jobID = jobSpecIDB
		close(delegateB.chContinueCreatingServices)

		eventuallyB.AwaitOrFail(t, 20*time.Second)
		mock.AssertExpectationsForObjects(t, serviceB1, serviceB2)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		serviceA1.On("Close").Return(nil).Once()
		serviceA2.On("Close").Return(nil).Once()
		require.NoError(t, spawner.DeleteJob(ctx, jobSpecIDA))

		serviceB1.On("Close").Return(nil).Once()
		serviceB2.On("Close").Return(nil).Once()
		require.NoError(t, spawner.DeleteJob(ctx, jobSpecIDB))

		require.NoError(t, spawner.Close())
		serviceA1.AssertExpectations(t)
		serviceA2.AssertExpectations(t)
		serviceB1.AssertExpectations(t)
		serviceB2.AssertExpectations(t)
	})

	t.Run("starts job services from the DB when .Start() is called", func(t *testing.T) {
		jobSpecA := makeOCRJobSpec(t, address)
		jobSpecA.Type = jobTypeA

		eventually := cltest.NewAwaiter()
		serviceA1 := new(mocks.Service)
		serviceA2 := new(mocks.Service)
		serviceA1.On("Start").Return(nil).Once()
		serviceA2.On("Start").Return(nil).Once().Run(func(mock.Arguments) { eventually.ItHappened() })

		orm := job.NewORM(db, config.Config, pipeline.NewORM(db, config, eventBroadcaster), eventBroadcaster, &postgres.NullAdvisoryLocker{})
		defer orm.Close()
		delegateA := &delegate{jobTypeA, []job.Service{serviceA1, serviceA2}, 0, nil, offchainreporting.NewDelegate(nil, orm, nil, nil, nil, eth.NewClientWith(rpc, geth), nil, nil, monitoringEndpoint)}
		spawner := job.NewSpawner(orm, config, map[job.Type]job.Delegate{
			jobTypeA: delegateA,
		})

		jobSpecIDA, err := spawner.CreateJob(context.Background(), *jobSpecA, null.String{})
		require.NoError(t, err)
		delegateA.jobID = jobSpecIDA

		spawner.Start()
		defer spawner.Close()

		eventually.AwaitOrFail(t, 10*time.Second)
		mock.AssertExpectationsForObjects(t, serviceA1, serviceA2)

		serviceA1.On("Close").Return(nil).Once()
		serviceA2.On("Close").Return(nil).Once()
	})

	t.Run("stops job services when .Stop() is called", func(t *testing.T) {
		jobSpecA := makeOCRJobSpec(t, address)
		jobSpecA.Type = jobTypeA

		eventually := cltest.NewAwaiter()
		serviceA1 := new(mocks.Service)
		serviceA2 := new(mocks.Service)
		serviceA1.On("Start").Return(nil).Once()
		serviceA2.On("Start").Return(nil).Once().Run(func(mock.Arguments) { eventually.ItHappened() })

		orm := job.NewORM(db, config.Config, pipeline.NewORM(db, config, eventBroadcaster), eventBroadcaster, &postgres.NullAdvisoryLocker{})
		defer orm.Close()
		delegateA := &delegate{jobTypeA, []job.Service{serviceA1, serviceA2}, 0, nil, offchainreporting.NewDelegate(nil, orm, nil, nil, nil, eth.NewClientWith(rpc, geth), nil, nil, monitoringEndpoint)}
		spawner := job.NewSpawner(orm, config, map[job.Type]job.Delegate{
			jobTypeA: delegateA,
		})

		jobSpecIDA, err := spawner.CreateJob(context.Background(), *jobSpecA, null.String{})
		require.NoError(t, err)
		delegateA.jobID = jobSpecIDA

		spawner.Start()

		eventually.AwaitOrFail(t, 10*time.Second)
		mock.AssertExpectationsForObjects(t, serviceA1, serviceA2)

		serviceA1.On("Close").Return(nil).Once()
		serviceA2.On("Close").Return(nil).Once()

		require.NoError(t, spawner.Close())

		mock.AssertExpectationsForObjects(t, serviceA1, serviceA2)
	})

	clearDB(t, db)

	t.Run("closes job services on 'delete_from_jobs' postgres event", func(t *testing.T) {
		jobSpecA := makeOCRJobSpec(t, address)
		jobSpecA.Type = jobTypeA

		eventuallyStart := cltest.NewAwaiter()
		serviceA1 := new(mocks.Service)
		serviceA2 := new(mocks.Service)
		serviceA1.On("Start").Return(nil).Once()
		serviceA2.On("Start").Return(nil).Once().Run(func(mock.Arguments) { eventuallyStart.ItHappened() })

		orm := job.NewORM(db, config.Config, pipeline.NewORM(db, config, eventBroadcaster), eventBroadcaster, &postgres.NullAdvisoryLocker{})
		defer orm.Close()
		delegateA := &delegate{jobTypeA, []job.Service{serviceA1, serviceA2}, 0, nil, offchainreporting.NewDelegate(nil, nil, nil, nil, nil, eth.NewClientWith(rpc, geth), nil, nil, monitoringEndpoint)}
		spawner := job.NewSpawner(orm, config, map[job.Type]job.Delegate{
			jobTypeA: delegateA,
		})

		jobSpecIDA, err := spawner.CreateJob(context.Background(), *jobSpecA, null.String{})
		require.NoError(t, err)
		delegateA.jobID = jobSpecIDA

		spawner.Start()
		defer spawner.Close()

		eventuallyStart.AwaitOrFail(t, 10*time.Second)

		advisoryLockClassID := job.GetORMAdvisoryLockClassID(orm)

		lock := struct{ Count int }{}
		// Wait for the claim lock to be taken
		gomega.NewGomegaWithT(t).Eventually(func() int {
			require.NoError(t, db.Raw(`SELECT count(*) AS count FROM pg_locks WHERE locktype = 'advisory' AND classid = ? AND objid = ?`, advisoryLockClassID, jobSpecIDA).Scan(&lock).Error)
			return lock.Count
		}, cltest.DBWaitTimeout, cltest.DBPollingInterval).Should(gomega.Equal(1))

		// Make sure that the job is claimed
		claimed := job.GetORMClaimedJobs(orm)
		assert.Len(t, claimed, 1)

		eventuallyClose := cltest.NewAwaiter()
		serviceA1.On("Close").Return(nil).Once()
		serviceA2.On("Close").Return(nil).Once().Run(func(mock.Arguments) { eventuallyClose.ItHappened() })

		require.NoError(t, db.Exec(`DELETE FROM jobs WHERE id = ?`, jobSpecIDA).Error)

		eventuallyClose.AwaitOrFail(t, 10*time.Second)

		// Wait for the claim lock to be released
		gomega.NewGomegaWithT(t).Eventually(func() int {
			require.NoError(t, db.Raw(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND classid = ? AND objid = ?`, advisoryLockClassID, jobSpecIDA).Scan(&lock).Error)
			return lock.Count
		}, cltest.DBWaitTimeout, cltest.DBPollingInterval).Should(gomega.Equal(1))

		// Make sure that the job is no longer claimed
		claimed = job.GetORMClaimedJobs(orm)
		require.Len(t, claimed, 0)

		mock.AssertExpectationsForObjects(t, serviceA1, serviceA2)
	})
}
