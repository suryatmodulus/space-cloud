package crud

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/graph-gophers/dataloader"
	"github.com/spaceuptech/helpers"

	"github.com/spaceuptech/space-cloud/gateway/config"
	"github.com/spaceuptech/space-cloud/gateway/managers/admin"
	"github.com/spaceuptech/space-cloud/gateway/model"
	"github.com/spaceuptech/space-cloud/gateway/modules/crud/bolt"
	"github.com/spaceuptech/space-cloud/gateway/utils"

	"github.com/spaceuptech/space-cloud/gateway/modules/crud/mgo"
	"github.com/spaceuptech/space-cloud/gateway/modules/crud/sql"
)

// Module is the root block providing convenient wrappers
type Module struct {
	sync.RWMutex

	dbType  string
	alias   string
	project string
	schema  model.SchemaCrudInterface
	auth    model.AuthCrudInterface
	queries map[string]*config.PreparedQuery
	// batch operation
	batchMapTableToChan batchMap // every table gets mapped to group of channels

	dataLoader loader
	// Variables to store the hooks
	hooks      *model.CrudHooks
	metricHook model.MetricCrudHook

	// Extra variables for enterprise
	blocks         map[string]Crud
	admin          *admin.Manager
	integrationMan integrationManagerInterface
	// function to get secrets from runner
	getSecrets utils.GetSecrets
}

type loader struct {
	loaderMap      map[string]*dataloader.Loader
	dataLoaderLock sync.RWMutex
}

// Crud abstracts the implementation crud operations of databases
type Crud interface {
	Create(ctx context.Context, col string, req *model.CreateRequest) (int64, error)
	Read(ctx context.Context, col string, req *model.ReadRequest) (int64, interface{}, error)
	Update(ctx context.Context, col string, req *model.UpdateRequest) (int64, error)
	Delete(ctx context.Context, col string, req *model.DeleteRequest) (int64, error)
	Aggregate(ctx context.Context, col string, req *model.AggregateRequest) (interface{}, error)
	Batch(ctx context.Context, req *model.BatchRequest) ([]int64, error)
	DescribeTable(ctc context.Context, col string) ([]model.InspectorFieldType, []model.ForeignKeysType, []model.IndexType, error)
	RawQuery(ctx context.Context, query string, args []interface{}) (int64, interface{}, error)
	GetCollections(ctx context.Context) ([]utils.DatabaseCollections, error)
	DeleteCollection(ctx context.Context, col string) error
	CreateDatabaseIfNotExist(ctx context.Context, name string) error
	RawBatch(ctx context.Context, batchedQueries []string) error
	GetDBType() model.DBType
	IsClientSafe(ctx context.Context) error
	IsSame(conn, dbName string) bool
	Close() error
	GetConnectionState(ctx context.Context) bool
}

// Init create a new instance of the Module object
func Init() *Module {
	return &Module{batchMapTableToChan: make(batchMap), blocks: map[string]Crud{}, dataLoader: loader{loaderMap: map[string]*dataloader.Loader{}}}
}

// SetSchema sets the schema module
func (m *Module) SetSchema(s model.SchemaCrudInterface) {
	m.schema = s
}

// SetAuth sets the auth module
func (m *Module) SetAuth(a model.AuthCrudInterface) {
	m.auth = a
}

// SetAdminManager sets the admin manager
func (m *Module) SetAdminManager(a *admin.Manager) {
	m.admin = a
}

// SetAdminManager sets the integration manager
func (m *Module) SetIntegrationManager(i integrationManagerInterface) {
	m.integrationMan = i
}

// SetHooks sets the internal hooks
func (m *Module) SetHooks(hooks *model.CrudHooks, metricHook model.MetricCrudHook) {
	m.hooks = hooks
	m.metricHook = metricHook
}

func (m *Module) initBlock(dbType model.DBType, enabled bool, connection, dbName string) (Crud, error) {
	switch dbType {
	case model.Mongo:
		return mgo.Init(enabled, connection, dbName)
	case model.EmbeddedDB:
		return bolt.Init(enabled, connection, dbName)
	case model.MySQL, model.Postgres, model.SQLServer:
		c, err := sql.Init(dbType, enabled, connection, dbName, m.auth)
		if err == nil && enabled {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.CreateDatabaseIfNotExist(ctx, dbName); err != nil {
				return nil, err
			}
		}
		if dbType == model.MySQL {
			return sql.Init(dbType, enabled, fmt.Sprintf("%s%s", connection, dbName), dbName, m.auth)
		}
		return c, err
	default:
		return nil, helpers.Logger.LogError(helpers.GetRequestID(context.TODO()), fmt.Sprintf("Unsupported database (%s) provided", dbType), nil, map[string]interface{}{})
	}
}

func (m *Module) getCrudBlock(dbAlias string) (Crud, error) {
	block, p := m.blocks[dbAlias]
	if !p {
		return nil, helpers.Logger.LogError(helpers.GetRequestID(context.TODO()), "Unable to get database connection", fmt.Errorf("crud module not initialized yet for %q", dbAlias), nil)
	}
	return block, nil
}

// SetConfig set the rules and secret key required by the crud block
func (m *Module) SetConfig(project string, crud config.Crud) error {
	m.Lock()
	defer m.Unlock()

	if err := m.admin.IsDBConfigValid(crud); err != nil {
		return err
	}

	m.project = project

	// Reset all existing prepared query
	m.queries = map[string]*config.PreparedQuery{}

	// clear previous data loader
	m.dataLoader = loader{loaderMap: map[string]*dataloader.Loader{}}

	// Create a new crud blocks
	for k, v := range crud {
		// Trim away the sql prefix for backward compatibility
		blockKey := strings.TrimPrefix(k, "sql-")

		if v.Type == "" {
			v.Type = k
		}

		// set default database name to project id
		if v.DBName == "" {
			v.DBName = project
		}

		// Add the prepared queries in this db
		for id, query := range v.PreparedQueries {
			m.queries[getPreparedQueryKey(strings.TrimPrefix(k, "sql-"), id)] = query
		}

		// check if connection string starts with secrets
		secretName, isSecretExists := splitConnectionString(v.Conn)
		connectionString := v.Conn
		if isSecretExists {
			var err error
			connectionString, err = m.getSecrets(project, secretName, "CONN")
			if err != nil {
				return helpers.Logger.LogError(helpers.GetRequestID(context.TODO()), "Unable to fetch secret from runner", err, map[string]interface{}{"project": project})
			}
		}

		if block, p := m.blocks[blockKey]; p {
			// Skip if the connection string is the same
			if block.IsSame(connectionString, v.DBName) {
				continue
			}
			// Close the previous database connection
			if err := block.Close(); err != nil {
				_ = helpers.Logger.LogError(helpers.GetRequestID(context.TODO()), "Unable to close database connections", err, map[string]interface{}{"project": project})
			}
		}

		var c Crud
		var err error

		v.Type = strings.TrimPrefix(v.Type, "sql-")
		c, err = m.initBlock(model.DBType(v.Type), v.Enabled, connectionString, v.DBName)

		if v.Enabled {
			if err != nil {
				return helpers.Logger.LogError(helpers.GetRequestID(context.TODO()), "Cannot connect to database", err, map[string]interface{}{"project": project, "dbAlias": k, "dbType": v.Type, "conn": v.Conn, "logicalDbName": v.DBName})
			}
			helpers.Logger.LogInfo(helpers.GetRequestID(context.TODO()), "Successfully connected to database", map[string]interface{}{"project": project, "dbAlias": k, "dbType": v.Type})
		}

		// Store the block
		m.dbType = v.Type
		m.blocks[blockKey] = c
		m.alias = blockKey
	}

	// Dont forget to delete the old crud blocks
	for k, block := range m.blocks {
		_, p1 := crud[k]
		_, p2 := crud["sql-"+k]
		if !p1 && !p2 {
			// Close the previous database connection
			if err := block.Close(); err != nil {
				_ = helpers.Logger.LogError(helpers.GetRequestID(context.TODO()), "Unable to close database connections", err, nil)
			}

			delete(m.blocks, k)
		}
	}

	m.closeBatchOperation()
	m.initBatchOperation(project, crud)
	return nil
}

// splitConnectionString splits the connection string
func splitConnectionString(connection string) (string, bool) {
	s := strings.Split(connection, ".")
	if s[0] == "secrets" {
		return s[1], true
	}
	return "", false
}

// GetDBType returns the type of the db for the alias provided
func (m *Module) GetDBType(dbAlias string) (string, error) {
	dbAlias = strings.TrimPrefix(dbAlias, "sql-")
	block, p := m.blocks[dbAlias]
	if !p {
		return "", fmt.Errorf("cannot get db type as invalid db alias (%s) provided", dbAlias)
	}

	return string(block.GetDBType()), nil
}

// SetGetSecrets sets the GetSecrets function
func (m *Module) SetGetSecrets(function utils.GetSecrets) {
	m.Lock()
	defer m.Unlock()

	m.getSecrets = function
}

// CloseConfig close the rules and secret key required by the crud block
func (m *Module) CloseConfig() error {
	// Acquire a lock
	m.Lock()
	defer m.Unlock()

	for k := range m.queries {
		delete(m.queries, k)
	}
	for k := range m.dataLoader.loaderMap {
		delete(m.dataLoader.loaderMap, k)
	}

	for _, block := range m.blocks {
		err := block.Close()
		if err != nil {
			return helpers.Logger.LogError(helpers.GetRequestID(context.TODO()), "Unable to close database connection", err, map[string]interface{}{})
		}
	}

	m.closeBatchOperation()

	return nil
}
