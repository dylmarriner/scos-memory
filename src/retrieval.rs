use crate::store::MemoryStore;
use crate::vector::VectorEmbedding;
use scos_common::{MemoryObject, MemoryType, Result};

/// Memory retrieval with semantic search.
pub struct MemoryRetrieval {
    store: MemoryStore,
}

impl MemoryRetrieval {
    pub fn new(store: MemoryStore) -> Self {
        Self { store }
    }

    /// Semantic search for memory objects using content pointer similarity.
    pub fn search(&self, query: &str, limit: usize) -> Result<Vec<MemoryObject>> {
        let all = self.store.all();

        let query_obj = MemoryObject {
            id: "query".to_string(),
            memory_type: MemoryType::Task,
            semantic_hash: query.to_string(),
            embedding_hash: None,
            owner_node: scos_common::NodeId("query".to_string()),
            content_pointer: query.to_string(),
            trust_score: 1.0,
            provenance: scos_common::MemoryProvenance {
                origin_node_id: scos_common::NodeId("query".to_string()),
                transaction_id: None,
                source_tool_id: None,
                prev_version_id: None,
            },
            graph_links: Vec::new(),
            created_at_ms: chrono::Utc::now().timestamp_millis() as u64,
            updated_at_ms: chrono::Utc::now().timestamp_millis() as u64,
        };

        let mut similar: Vec<_> = all
            .into_iter()
            .map(|obj| {
                let similarity = VectorEmbedding::similarity(&query_obj, &obj);
                (obj, similarity)
            })
            .filter(|(_, sim)| *sim > 0.0)
            .collect();

        similar.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));

        Ok(similar
            .into_iter()
            .take(limit)
            .map(|(obj, _)| obj)
            .collect())
    }

    /// Retrieve memory by ID.
    pub fn get(&self, id: &str) -> Option<MemoryObject> {
        self.store.get(id)
    }

    /// Find similar memories to a given memory object using vector similarity.
    pub fn find_similar(&self, obj: &MemoryObject, limit: usize) -> Result<Vec<MemoryObject>> {
        let all = self.store.all();

        let mut similar: Vec<_> = all
            .into_iter()
            .filter(|m| m.id != obj.id)
            .map(|m| {
                let similarity = VectorEmbedding::similarity(obj, &m);
                (m, similarity)
            })
            .collect();

        similar.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));

        Ok(similar
            .into_iter()
            .take(limit)
            .map(|(obj, _)| obj)
            .collect())
    }
}
