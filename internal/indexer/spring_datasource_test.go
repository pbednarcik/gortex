package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// springJavaGraphPath is the graph path of the fixture's Java file. The
// indexer keys graph paths — and therefore the symbol IDs built from them —
// with pathkey.Normalize(filepath.ToSlash(rel)), so a node ID carries forward
// slashes on every platform. filepath.Join here would spell `src\main\...`
// on Windows and match no node.
const springJavaGraphPath = "src/main/java/com/example/JdbcConfig.java"

func TestIndex_SpringDatasourceConditionalOnPropertyReadsConfig(t *testing.T) {
	dir := t.TempDir()
	javaDir := filepath.Join(dir, "src", "main", "java", "com", "example")
	resDir := filepath.Join(dir, "src", "main", "resources")
	require.NoError(t, os.MkdirAll(javaDir, 0o755))
	require.NoError(t, os.MkdirAll(resDir, 0o755))

	writeFile(t, filepath.Join(javaDir, "JdbcConfig.java"), `
package com.example;

import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.context.annotation.Configuration;

@Configuration
@ConditionalOnProperty(prefix = "spring.datasource", name = "url")
public class JdbcConfig {
}
`)
	writeFile(t, filepath.Join(resDir, "application.yml"), `
spring:
  datasource:
    url: jdbc:h2:mem:test
    username: sa
`)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	javaGraphPath := springJavaGraphPath
	cfgID := javaGraphPath + "::JdbcConfig"
	keyID := "cfg::spring::spring.datasource.url"
	if g.GetNode(keyID) == nil {
		t.Logf("file nodes: %v", nodeIDsOfKind(g, graph.KindFile))
		t.Logf("config nodes: %v", nodeIDsOfKind(g, graph.KindConfigKey))
	}
	require.NotNil(t, g.GetNode(keyID), "spring.datasource.url config key should be indexed")
	require.True(t, hasEdge(g, cfgID, keyID, graph.EdgeReadsConfig),
		"JdbcConfig should read spring.datasource.url via @ConditionalOnProperty")
}

func TestIndex_SpringExplicitDataSourceBeanLinksToBeanConsumers(t *testing.T) {
	dir := t.TempDir()
	javaDir := filepath.Join(dir, "src", "main", "java", "com", "example")
	require.NoError(t, os.MkdirAll(javaDir, 0o755))

	writeFile(t, filepath.Join(javaDir, "JdbcConfig.java"), `
package com.example;

import javax.sql.DataSource;
import org.springframework.context.annotation.Bean;
import org.springframework.jdbc.core.JdbcTemplate;

public class JdbcConfig {
    @Bean
    DataSource dataSource() {
        return null;
    }

    @Bean
    JdbcTemplate jdbcTemplate(DataSource dataSource) {
        return new JdbcTemplate(dataSource);
    }
}
`)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	javaGraphPath := springJavaGraphPath
	classID := javaGraphPath + "::JdbcConfig"
	dataSourceID := classID + ".dataSource"
	if !hasEdge(g, classID, dataSourceID, graph.EdgeCalls) {
		t.Logf("calls edges: %v", edgePairsOfKind(g, graph.EdgeCalls))
		t.Logf("provides edges: %v", edgePairsOfKind(g, graph.EdgeProvides))
	}
	require.True(t, hasEdge(g, classID, dataSourceID, graph.EdgeCalls),
		"JdbcConfig should have a spring.Bean edge to the DataSource factory because jdbcTemplate consumes DataSource")
}

func TestIndex_SpringDataSourcePlainMethodDoesNotLinkBeanFactory(t *testing.T) {
	dir := t.TempDir()
	javaDir := filepath.Join(dir, "src", "main", "java", "com", "example")
	require.NoError(t, os.MkdirAll(javaDir, 0o755))

	writeFile(t, filepath.Join(javaDir, "JdbcConfig.java"), `
package com.example;

import javax.sql.DataSource;
import org.springframework.context.annotation.Bean;

public class JdbcConfig {
    @Bean
    DataSource dataSource() {
        return null;
    }

    void inspect(DataSource dataSource) {
    }
}
`)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	javaGraphPath := springJavaGraphPath
	classID := javaGraphPath + "::JdbcConfig"
	dataSourceID := classID + ".dataSource"
	if hasEdge(g, classID, dataSourceID, graph.EdgeCalls) {
		t.Logf("calls edges: %v", edgePairsOfKind(g, graph.EdgeCalls))
	}
	require.False(t, hasEdge(g, classID, dataSourceID, graph.EdgeCalls),
		"a plain method parameter should not make the class look like a Spring bean consumer")
}

func hasEdge(g *graph.Graph, from, to string, kind graph.EdgeKind) bool {
	for _, e := range g.AllEdges() {
		if e.From == from && e.To == to && e.Kind == kind {
			return true
		}
	}
	return false
}

func nodeIDsOfKind(g *graph.Graph, kind graph.NodeKind) []string {
	var out []string
	for _, n := range g.AllNodes() {
		if n.Kind == kind {
			out = append(out, n.ID)
		}
	}
	return out
}

func edgePairsOfKind(g *graph.Graph, kind graph.EdgeKind) []string {
	var out []string
	for _, e := range g.AllEdges() {
		if e.Kind == kind {
			out = append(out, e.From+"->"+e.To)
		}
	}
	return out
}
