import os
import sys
import requests

# --- Configuration ---
BASE_URL = "https://app.asana.com/api/1.0"

def get_task_gid_by_name(project_id, task_name, headers):
    """Searches a project for a specific task name and returns its GID."""
    print(f"🔍 Searching project for main task: '{task_name}'...")
    url = f"{BASE_URL}/projects/{project_id}/tasks"
    params = {"opt_fields": "name,gid"} 
    
    response = requests.get(url, headers=headers, params=params)
    response.raise_for_status()
    
    for task in response.json().get("data", []):
        if task.get("name") == task_name:
            print(f"✅ Found main task: {task['name']} (GID: {task['gid']})")
            return task["gid"]
            
    print(f"❌ Error: Main task '{task_name}' not found.")
    return None

def get_subtask_gid_by_name(parent_task_id, subtask_name, headers):
    """Fetches all subtasks for a given task and returns the GID of the matching name."""
    print(f"🔍 Searching for subtask: '{subtask_name}'...")
    url = f"{BASE_URL}/tasks/{parent_task_id}/subtasks"
    params = {"opt_fields": "name,gid"} 
    
    response = requests.get(url, headers=headers, params=params)
    response.raise_for_status()
    
    for subtask in response.json().get("data", []):
        if subtask.get("name") == subtask_name:
            print(f"✅ Found subtask: {subtask['name']} (GID: {subtask['gid']})")
            return subtask["gid"]
            
    print(f"❌ Error: Subtask '{subtask_name}' not found.")
    return None

def complete_task(task_id, headers):
    """Marks a task or subtask as completed."""
    print(f"🔄 Marking task {task_id} as complete...")
    url = f"{BASE_URL}/tasks/{task_id}"
    payload = {"data": {"completed": True}}
    
    response = requests.put(url, headers=headers, json=payload)
    response.raise_for_status()
    print("🎉 Successfully checked the box in Asana!")

def main():
    # 1. Fetch Environment Variables
    asana_pat = os.environ.get("ASANA_PAT")
    project_gid = os.environ.get("ASANA_PROJECT_GID")
    release_version = os.environ.get("RELEASE_VERSION")
    
    # Defaults to the branch creation step, but can be overridden by different CI jobs
    target_subtask_name = os.environ.get(
        "TARGET_SUBTASK_NAME", 
        "Create Release Branch From develop"
    )

    # 2. Pre-flight Validation
    if not all([asana_pat, project_gid, release_version]):
        print("❌ CRITICAL: Missing required environment variables.")
        print("Ensure ASANA_PAT, ASANA_PROJECT_GID, and RELEASE_VERSION are set.")
        sys.exit(1)

    headers = {
        "Authorization": f"Bearer {asana_pat}",
        "Accept": "application/json",
        "Content-Type": "application/json"
    }
    
    target_task_name = f"Release {release_version}"

    try:
        # 3. Find the main release task (e.g., "Release v1.4.0")
        main_task_gid = get_task_gid_by_name(project_gid, target_task_name, headers)
        if not main_task_gid:
            sys.exit(1)
            
        # 4. Find the specific subtask (e.g., "Create Release Branch From develop")
        subtask_gid = get_subtask_gid_by_name(main_task_gid, target_subtask_name, headers)
        if not subtask_gid:
            sys.exit(1)
            
        # 5. Check the box
        complete_task(subtask_gid, headers)
            
    except requests.exceptions.HTTPError as err:
        print(f"❌ API Request failed with status code {err.response.status_code}")
        print(f"Response body: {err.response.text}")
        sys.exit(1)
    except Exception as e:
        print(f"❌ An unexpected error occurred: {e}")
        sys.exit(1)

if __name__ == "__main__":
    main()