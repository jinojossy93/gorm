package migrator

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Migrator m struct
type Migrator struct {
	Config
}

// Config schema config
type Config struct {
	CreateIndexAfterCreateTable             bool
	AllowDeferredConstraintsWhenAutoMigrate bool
	DB                                      *gorm.DB
	gorm.Dialector
}

type GormDataTypeInterface interface {
	GormDBDataType(*gorm.DB, *schema.Field) string
}

func (m Migrator) RunWithValue(value interface{}, fc func(*gorm.Statement) error) error {
	stmt := &gorm.Statement{DB: m.DB}
	if m.DB.Statement != nil {
		stmt.Table = m.DB.Statement.Table
	}

	if table, ok := value.(string); ok {
		stmt.Table = table
	} else if err := stmt.Parse(value); err != nil {
		return err
	}

	return fc(stmt)
}

func (m Migrator) DataTypeOf(field *schema.Field) string {
	if field.DBDataType != "" {
		return field.DBDataType
	}

	fieldValue := reflect.New(field.IndirectFieldType)
	if dataTyper, ok := fieldValue.Interface().(GormDataTypeInterface); ok {
		if dataType := dataTyper.GormDBDataType(m.DB, field); dataType != "" {
			return dataType
		}
	}

	return m.Dialector.DataTypeOf(field)
}

func (m Migrator) FullDataTypeOf(field *schema.Field) (expr clause.Expr) {
	expr.SQL = m.DataTypeOf(field)

	if field.AutoIncrement {
		expr.SQL += " AUTO_INCREMENT"
	}

	if field.NotNull {
		expr.SQL += " NOT NULL"
	}

	if field.Unique {
		expr.SQL += " UNIQUE"
	}

	if field.HasDefaultValue && field.DefaultValue != "" {
		if field.DataType == schema.String {
			defaultStmt := &gorm.Statement{Vars: []interface{}{field.DefaultValue}}
			m.Dialector.BindVarTo(defaultStmt, defaultStmt, field.DefaultValue)
			expr.SQL += " DEFAULT " + m.Dialector.Explain(defaultStmt.SQL.String(), field.DefaultValue)
		} else {
			expr.SQL += " DEFAULT " + field.DefaultValue
		}
	}

	return
}

// AutoMigrate
func (m Migrator) AutoMigrate(values ...interface{}) error {
	// TODO smart migrate data type
	for _, value := range m.ReorderModels(values, true) {
		tx := m.DB.Session(&gorm.Session{})
		if !tx.Migrator().HasTable(value) {
			if err := tx.Migrator().CreateTable(value); err != nil {
				return err
			}
		} else {
			if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
				for _, field := range stmt.Schema.FieldsByDBName {
					if !tx.Migrator().HasColumn(value, field.DBName) {
						if err := tx.Migrator().AddColumn(value, field.DBName); err != nil {
							return err
						}
					}
				}

				for _, rel := range stmt.Schema.Relationships.Relations {
					if constraint := rel.ParseConstraint(); constraint != nil {
						if !tx.Migrator().HasConstraint(value, constraint.Name) {
							if err := tx.Migrator().CreateConstraint(value, constraint.Name); err != nil {
								return err
							}
						}
					}

					for _, chk := range stmt.Schema.ParseCheckConstraints() {
						if !tx.Migrator().HasConstraint(value, chk.Name) {
							if err := tx.Migrator().CreateConstraint(value, chk.Name); err != nil {
								return err
							}
						}
					}

					// create join table
					if rel.JoinTable != nil {
						joinValue := reflect.New(rel.JoinTable.ModelType).Interface()
						if !tx.Migrator().HasTable(rel.JoinTable.Table) {
							defer tx.Table(rel.JoinTable.Table).Migrator().CreateTable(joinValue)
						} else {
							defer tx.Table(rel.JoinTable.Table).Migrator().AutoMigrate(joinValue)
						}
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func (m Migrator) CreateTable(values ...interface{}) error {
	for _, value := range m.ReorderModels(values, false) {
		tx := m.DB.Session(&gorm.Session{})
		if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
			var (
				createTableSQL          = "CREATE TABLE ? ("
				values                  = []interface{}{clause.Table{Name: stmt.Table}}
				hasPrimaryKeyInDataType bool
			)

			for _, dbName := range stmt.Schema.DBNames {
				field := stmt.Schema.FieldsByDBName[dbName]
				createTableSQL += fmt.Sprintf("? ?")
				hasPrimaryKeyInDataType = hasPrimaryKeyInDataType || strings.Contains(strings.ToUpper(field.DBDataType), "PRIMARY KEY")
				values = append(values, clause.Column{Name: dbName}, m.FullDataTypeOf(field))
				createTableSQL += ","
			}

			if !hasPrimaryKeyInDataType && len(stmt.Schema.PrimaryFields) > 0 {
				createTableSQL += "PRIMARY KEY ?,"
				primaryKeys := []interface{}{}
				for _, field := range stmt.Schema.PrimaryFields {
					primaryKeys = append(primaryKeys, clause.Column{Name: field.DBName})
				}

				values = append(values, primaryKeys)
			}

			for _, idx := range stmt.Schema.ParseIndexes() {
				if m.CreateIndexAfterCreateTable {
					defer tx.Migrator().CreateIndex(value, idx.Name)
				} else {
					createTableSQL += "INDEX ? ?,"
					values = append(values, clause.Expr{SQL: idx.Name}, tx.Migrator().(BuildIndexOptionsInterface).BuildIndexOptions(idx.Fields, stmt))
				}
			}

			for _, rel := range stmt.Schema.Relationships.Relations {
				if constraint := rel.ParseConstraint(); constraint != nil {
					sql, vars := buildConstraint(constraint)
					createTableSQL += sql + ","
					values = append(values, vars...)
				}

				// create join table
				if rel.JoinTable != nil {
					joinValue := reflect.New(rel.JoinTable.ModelType).Interface()
					if !tx.Migrator().HasTable(rel.JoinTable.Table) {
						defer tx.Table(rel.JoinTable.Table).Migrator().CreateTable(joinValue)
					}
				}
			}

			for _, chk := range stmt.Schema.ParseCheckConstraints() {
				createTableSQL += "CONSTRAINT ? CHECK ?,"
				values = append(values, clause.Column{Name: chk.Name}, clause.Expr{SQL: chk.Constraint})
			}

			createTableSQL = strings.TrimSuffix(createTableSQL, ",")

			createTableSQL += ")"

			if tableOption, ok := m.DB.Get("gorm:table_options"); ok {
				createTableSQL += fmt.Sprint(tableOption)
			}

			return tx.Exec(createTableSQL, values...).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m Migrator) DropTable(values ...interface{}) error {
	values = m.ReorderModels(values, false)
	for i := len(values) - 1; i >= 0; i-- {
		tx := m.DB.Session(&gorm.Session{})
		if err := m.RunWithValue(values[i], func(stmt *gorm.Statement) error {
			return tx.Exec("DROP TABLE IF EXISTS ?", clause.Table{Name: stmt.Table}).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m Migrator) HasTable(value interface{}) bool {
	var count int64

	m.RunWithValue(value, func(stmt *gorm.Statement) error {
		currentDatabase := m.DB.Migrator().CurrentDatabase()
		return m.DB.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ? AND table_type = ?", currentDatabase, stmt.Table, "BASE TABLE").Row().Scan(&count)
	})

	return count > 0
}

func (m Migrator) RenameTable(oldName, newName interface{}) error {
	var oldTable, newTable string
	if v, ok := oldName.(string); ok {
		oldTable = v
	} else {
		stmt := &gorm.Statement{DB: m.DB}
		if err := stmt.Parse(oldName); err == nil {
			oldTable = stmt.Table
		} else {
			return err
		}
	}

	if v, ok := newName.(string); ok {
		newTable = v
	} else {
		stmt := &gorm.Statement{DB: m.DB}
		if err := stmt.Parse(newName); err == nil {
			newTable = stmt.Table
		} else {
			return err
		}
	}

	return m.DB.Exec("ALTER TABLE ? RENAME TO ?", clause.Table{Name: oldTable}, clause.Table{Name: newTable}).Error
}

func (m Migrator) AddColumn(value interface{}, field string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if field := stmt.Schema.LookUpField(field); field != nil {
			return m.DB.Exec(
				"ALTER TABLE ? ADD ? ?",
				clause.Table{Name: stmt.Table}, clause.Column{Name: field.DBName}, m.FullDataTypeOf(field),
			).Error
		}
		return fmt.Errorf("failed to look up field with name: %s", field)
	})
}

func (m Migrator) DropColumn(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if field := stmt.Schema.LookUpField(name); field != nil {
			name = field.DBName
		}

		return m.DB.Exec(
			"ALTER TABLE ? DROP COLUMN ?", clause.Table{Name: stmt.Table}, clause.Column{Name: name},
		).Error
	})
}

func (m Migrator) AlterColumn(value interface{}, field string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if field := stmt.Schema.LookUpField(field); field != nil {
			return m.DB.Exec(
				"ALTER TABLE ? ALTER COLUMN ? TYPE ?",
				clause.Table{Name: stmt.Table}, clause.Column{Name: field.DBName}, m.FullDataTypeOf(field),
			).Error
		}
		return fmt.Errorf("failed to look up field with name: %s", field)
	})
}

func (m Migrator) HasColumn(value interface{}, field string) bool {
	var count int64
	m.RunWithValue(value, func(stmt *gorm.Statement) error {
		currentDatabase := m.DB.Migrator().CurrentDatabase()
		name := field
		if field := stmt.Schema.LookUpField(field); field != nil {
			name = field.DBName
		}

		return m.DB.Raw(
			"SELECT count(*) FROM INFORMATION_SCHEMA.columns WHERE table_schema = ? AND table_name = ? AND column_name = ?",
			currentDatabase, stmt.Table, name,
		).Row().Scan(&count)
	})

	return count > 0
}

func (m Migrator) RenameColumn(value interface{}, oldName, newName string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if field := stmt.Schema.LookUpField(oldName); field != nil {
			oldName = field.DBName
		}

		if field := stmt.Schema.LookUpField(newName); field != nil {
			newName = field.DBName
		}

		return m.DB.Exec(
			"ALTER TABLE ? RENAME COLUMN ? TO ?",
			clause.Table{Name: stmt.Table}, clause.Column{Name: oldName}, clause.Column{Name: newName},
		).Error
	})
}

func (m Migrator) ColumnTypes(value interface{}) (columnTypes []*sql.ColumnType, err error) {
	err = m.RunWithValue(value, func(stmt *gorm.Statement) error {
		rows, err := m.DB.Raw("select * from ?", clause.Table{Name: stmt.Table}).Rows()
		if err == nil {
			columnTypes, err = rows.ColumnTypes()
		}
		return err
	})
	return
}

func (m Migrator) CreateView(name string, option gorm.ViewOption) error {
	return gorm.ErrNotImplemented
}

func (m Migrator) DropView(name string) error {
	return gorm.ErrNotImplemented
}

func buildConstraint(constraint *schema.Constraint) (sql string, results []interface{}) {
	sql = "CONSTRAINT ? FOREIGN KEY ? REFERENCES ??"
	if constraint.OnDelete != "" {
		sql += " ON DELETE " + constraint.OnDelete
	}

	if constraint.OnUpdate != "" {
		sql += " ON UPDATE  " + constraint.OnUpdate
	}

	var foreignKeys, references []interface{}
	for _, field := range constraint.ForeignKeys {
		foreignKeys = append(foreignKeys, clause.Column{Name: field.DBName})
	}

	for _, field := range constraint.References {
		references = append(references, clause.Column{Name: field.DBName})
	}
	results = append(results, clause.Table{Name: constraint.Name}, foreignKeys, clause.Table{Name: constraint.ReferenceSchema.Table}, references)
	return
}

func (m Migrator) CreateConstraint(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		checkConstraints := stmt.Schema.ParseCheckConstraints()
		if chk, ok := checkConstraints[name]; ok {
			return m.DB.Exec(
				"ALTER TABLE ? ADD CONSTRAINT ? CHECK ?",
				clause.Table{Name: stmt.Table}, clause.Column{Name: chk.Name}, clause.Expr{SQL: chk.Constraint},
			).Error
		}

		for _, rel := range stmt.Schema.Relationships.Relations {
			if constraint := rel.ParseConstraint(); constraint != nil && constraint.Name == name {
				sql, values := buildConstraint(constraint)
				return m.DB.Exec("ALTER TABLE ? ADD "+sql, append([]interface{}{clause.Table{Name: stmt.Table}}, values...)...).Error
			}
		}

		err := fmt.Errorf("failed to create constraint with name %v", name)
		if field := stmt.Schema.LookUpField(name); field != nil {
			for _, cc := range checkConstraints {
				if err = m.DB.Migrator().CreateIndex(value, cc.Name); err != nil {
					return err
				}
			}

			for _, rel := range stmt.Schema.Relationships.Relations {
				if constraint := rel.ParseConstraint(); constraint != nil && constraint.Field == field {
					if err = m.DB.Migrator().CreateIndex(value, constraint.Name); err != nil {
						return err
					}
				}
			}
		}

		return err
	})
}

func (m Migrator) DropConstraint(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		return m.DB.Exec(
			"ALTER TABLE ? DROP CONSTRAINT ?",
			clause.Table{Name: stmt.Table}, clause.Column{Name: name},
		).Error
	})
}

func (m Migrator) HasConstraint(value interface{}, name string) bool {
	var count int64
	m.RunWithValue(value, func(stmt *gorm.Statement) error {
		currentDatabase := m.DB.Migrator().CurrentDatabase()
		return m.DB.Raw(
			"SELECT count(*) FROM INFORMATION_SCHEMA.referential_constraints WHERE constraint_schema = ? AND table_name = ? AND constraint_name = ?",
			currentDatabase, stmt.Table, name,
		).Row().Scan(&count)
	})

	return count > 0
}

func (m Migrator) BuildIndexOptions(opts []schema.IndexOption, stmt *gorm.Statement) (results []interface{}) {
	for _, opt := range opts {
		str := stmt.Quote(opt.DBName)
		if opt.Expression != "" {
			str = opt.Expression
		} else if opt.Length > 0 {
			str += fmt.Sprintf("(%d)", opt.Length)
		}

		if opt.Collate != "" {
			str += " COLLATE " + opt.Collate
		}

		if opt.Sort != "" {
			str += " " + opt.Sort
		}
		results = append(results, clause.Expr{SQL: str})
	}
	return
}

type BuildIndexOptionsInterface interface {
	BuildIndexOptions([]schema.IndexOption, *gorm.Statement) []interface{}
}

func (m Migrator) CreateIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if idx := stmt.Schema.LookIndex(name); idx != nil {
			opts := m.DB.Migrator().(BuildIndexOptionsInterface).BuildIndexOptions(idx.Fields, stmt)
			values := []interface{}{clause.Column{Name: idx.Name}, clause.Table{Name: stmt.Table}, opts}

			createIndexSQL := "CREATE "
			if idx.Class != "" {
				createIndexSQL += idx.Class + " "
			}
			createIndexSQL += "INDEX ? ON ??"

			if idx.Comment != "" {
				values = append(values, idx.Comment)
				createIndexSQL += " COMMENT ?"
			}

			if idx.Type != "" {
				createIndexSQL += " USING " + idx.Type
			}

			return m.DB.Exec(createIndexSQL, values...).Error
		}

		return fmt.Errorf("failed to create index with name %v", name)
	})
}

func (m Migrator) DropIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if idx := stmt.Schema.LookIndex(name); idx != nil {
			name = idx.Name
		}

		return m.DB.Exec("DROP INDEX ? ON ?", clause.Column{Name: name}, clause.Table{Name: stmt.Table}).Error
	})
}

func (m Migrator) HasIndex(value interface{}, name string) bool {
	var count int64
	m.RunWithValue(value, func(stmt *gorm.Statement) error {
		currentDatabase := m.DB.Migrator().CurrentDatabase()
		if idx := stmt.Schema.LookIndex(name); idx != nil {
			name = idx.Name
		}

		return m.DB.Raw(
			"SELECT count(*) FROM information_schema.statistics WHERE table_schema = ? AND table_name = ? AND index_name = ?",
			currentDatabase, stmt.Table, name,
		).Row().Scan(&count)
	})

	return count > 0
}

func (m Migrator) RenameIndex(value interface{}, oldName, newName string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		return m.DB.Exec(
			"ALTER TABLE ? RENAME INDEX ? TO ?",
			clause.Table{Name: stmt.Table}, clause.Column{Name: oldName}, clause.Column{Name: newName},
		).Error
	})
}

func (m Migrator) CurrentDatabase() (name string) {
	m.DB.Raw("SELECT DATABASE()").Row().Scan(&name)
	return
}

// ReorderModels reorder models according to constraint dependencies
func (m Migrator) ReorderModels(values []interface{}, autoAdd bool) (results []interface{}) {
	type Dependency struct {
		*gorm.Statement
		Depends []*schema.Schema
	}

	var (
		modelNames, orderedModelNames []string
		orderedModelNamesMap          = map[string]bool{}
		valuesMap                     = map[string]Dependency{}
		insertIntoOrderedList         func(name string)
	)

	parseDependence := func(value interface{}, addToList bool) {
		dep := Dependency{
			Statement: &gorm.Statement{DB: m.DB, Dest: value},
		}
		dep.Parse(value)

		for _, rel := range dep.Schema.Relationships.Relations {
			if c := rel.ParseConstraint(); c != nil && c.Schema != c.ReferenceSchema {
				dep.Depends = append(dep.Depends, c.ReferenceSchema)
			}
		}

		valuesMap[dep.Schema.Table] = dep

		if addToList {
			modelNames = append(modelNames, dep.Schema.Table)
		}
	}

	insertIntoOrderedList = func(name string) {
		if _, ok := orderedModelNamesMap[name]; ok {
			return // avoid loop
		}

		dep := valuesMap[name]
		for _, d := range dep.Depends {
			if _, ok := valuesMap[d.Table]; ok {
				insertIntoOrderedList(d.Table)
			} else if autoAdd {
				parseDependence(reflect.New(d.ModelType).Interface(), autoAdd)
				insertIntoOrderedList(d.Table)
			}
		}

		orderedModelNames = append(orderedModelNames, name)
		orderedModelNamesMap[name] = true
	}

	for _, value := range values {
		if v, ok := value.(string); ok {
			results = append(results, v)
		} else {
			parseDependence(value, true)
		}
	}

	for _, name := range modelNames {
		insertIntoOrderedList(name)
	}

	for _, name := range orderedModelNames {
		results = append(results, valuesMap[name].Statement.Dest)
	}
	return
}
