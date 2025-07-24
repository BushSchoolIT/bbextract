import os
import sys
import io
import subprocess
import pathlib
from multiprocessing import Process

# SETUP environment BEFORE importing prefect (important)
os.environ["PREFECT_API_URL"] = "http://localhost:4200/api"
WORK_POOL = "work_pool_0"

from prefect import flow, task, get_run_logger
from prefect.task_runners import SequentialTaskRunner
from prefect.deployments import Deployment

sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding="utf-8", errors="replace")

BASE_DIR = pathlib.Path(__file__).parent.resolve()
EXTRACTOR_PATH = os.getenv("EXTRACTOR_PATH", str(BASE_DIR / "bbextract"))

def start_worker():
    subprocess.run(["prefect", "worker", "start","--pool", WORK_POOL])

def run_exe(args: list[str]):
    exe_path = args[0]
    exe_dir = os.path.dirname(exe_path)
    name = os.path.basename(exe_path)
    logger = get_run_logger()

    logger.info(f"Running cmd: {[exe_path] + args[1:]}")

    # Use Popen for streaming output
    process = subprocess.Popen(
        [exe_path] + args[1:],
        cwd=exe_dir,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,  # merge stderr into stdout
        text=True,
        bufsize=1,  # line-buffered
        universal_newlines=True
    )

    # Stream output line by line
    if process.stdout is None:
        raise Exception("stdout is none, bad type")

    for line in process.stdout:
        logger.info(line.rstrip())

    process.stdout.close()
    return_code = process.wait()

    if return_code != 0:
        logger.error(f"{name} failed with exit code {return_code}")
        raise subprocess.CalledProcessError(return_code, [exe_path] + args[1:])

    logger.info(f"{name} completed successfully.")



@task
def transcripts_task_go():
    run_exe([EXTRACTOR_PATH, "transcripts"])

@task
def gpa_task_go():
    run_exe([EXTRACTOR_PATH, "gpa"])

@task
def enrollment_task_go():
    run_exe([EXTRACTOR_PATH, "enrollment"])

@task
def gsync_task_go():
    run_exe([EXTRACTOR_PATH, "gsync-students"])

@task 
def comments_task_go():
    run_exe([EXTRACTOR_PATH, "comments"])

@task
def parents_task_go():
    run_exe([EXTRACTOR_PATH, "parents"])

@task
def mailsync_task_go():
    run_exe([EXTRACTOR_PATH, "mailsync"])


@flow(task_runner=SequentialTaskRunner())
def run_mailsync_go():
    parents_task_go()
    mailsync_task_go()

@flow(task_runner=SequentialTaskRunner())
def run_enrollment_gsync():
    enrollment_task_go()
    gsync_task_go()

@flow
def run_attendance_go():
    run_exe([EXTRACTOR_PATH, "attendance"])

@flow(task_runner=SequentialTaskRunner())
def run_transcripts_go():
    transcripts_task_go()
    comments_task_go()
    gpa_task_go()

if __name__ == "__main__":
    Deployment.build_from_flow(
        flow=run_attendance_go,
        name="run_attendance_go",
        work_pool_name=WORK_POOL,
        schedule={"interval": 86400}
    ).apply()

    Deployment.build_from_flow(
        flow=run_transcripts_go,
        name="run_transcripts_go",
        work_pool_name=WORK_POOL,
        schedule={"interval": 86400}
    ).apply()

    Deployment.build_from_flow(
        flow=run_mailsync_go,
        name="run_mailsync_go",
        work_pool_name=WORK_POOL,
        schedule={"interval": 86400}
    ).apply()

    p = Process(target=start_worker)
    p.start()
    p.join()
