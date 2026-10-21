use parking_lot::Mutex;
use scos_common::{MemoryObject, MemoryType, NodeId, Result};
use std::collections::HashMap;
use std::path::Path;
use std::sync::Arc;

/// Local memory persistence layer.
/// In-memory implementation for MVP (Phase 6).
pub struct MemoryStore {
    objects: Arc<Mutex<HashMap<String, MemoryObject>>>,
}

impl MemoryStore {
    /// Open or create the memory store at the given directory.
    pub fn open(_dir: &Path) -> Result<Self> {
        // For MVP, use in-memory storage
        // In production, this would use SQLite or sled
        Ok(Self {
            objects: Arc::new(Mutex::new(HashMap::new())),
        })
    }

    /// Insert a memory object.
    pub fn insert(&self, obj: &MemoryObject) -> Result<()> {
        let mut objects = self.objects.lock();
        objects.insert(obj.id.clone(), obj.clone());
        Ok(())
    }

    /// Get a memory object by ID.
    pub fn get(&self, id: &str) -> Option<MemoryObject> {
        let objects = self.objects.lock();
        objects.get(id).cloned()
    }

    /// Query memory objects by owner.
    pub fn query_by_owner(&self, owner: &NodeId) -> Vec<MemoryObject> {
        let objects = self.objects.lock();
        objects
            .values()
            .filter(|obj| obj.owner_node == *owner)
            .cloned()
            .collect()
    }

    /// Query memory objects by type.
    pub fn query_by_type(&self, memory_type: MemoryType) -> Vec<MemoryObject> {
        let objects = self.objects.lock();
        objects
            .values()
            .filter(|obj| {
                std::mem::discriminant(&obj.memory_type) == std::mem::discriminant(&memory_type)
            })
            .cloned()
            .collect()
    }

    /// Delete a memory object.
    pub fn delete(&self, id: &str) -> Result<()> {
        let mut objects = self.objects.lock();
        objects.remove(id);
        Ok(())
    }

    /// Get all memory objects.
    pub fn all(&self) -> Vec<MemoryObject> {
        let objects = self.objects.lock();
        objects.values().cloned().collect()
    }
}
