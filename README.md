# bbextract
General purpose tool for various syncing and ETL tasks accross Bush

## Features/Overview:
* `bbextract gsync-students` sync students from DB->Google Admin, automatically creates OUs, suspends users, etc
* `bbextract attendance` extracts Attendance data from BlackBaud->DB
* `bbextract transcripts` extracts Transcripts data from BlackBaud->DB and transforms it for the PowerBI integration to generate the full transcripts
  * `bbextract gpa` populates GPA info in the DB from the transcripts info fetched from blackbaud
  * `bbextract comments` fetches Transcripts Comments from BlackBaud->DB
* `bbextract mailsync` syncs parent emails fetched from blackbaud in the DB->Email Octopus

Basically, we try to treat blackbaud as the "source of truth", fetch all the info we can from it, put it into a usable/queryable format in the DB. This DB is then used to sync with various different services, like the PowerBI transcript generation, Email Octopus Parent Emails, and students in the google admin API.

Each of these are ran on a "schedule" where there is only one worker at a time accessing the database through prefect

## Setup
download the bbextract binary and setup the following files

download the files flow.bat, flow.py, and server.py and put them all in the same directory, call it `bbextract`, you can use NSSM in order to setup the flow.bat and server.bat as services, flow.bat should *depend* on server.bat.

Authfiles:
Authentication files for various services, `octo_auth.json` should be something like:
### octo_auth.json
```json
{
  "key": "API_KEY_FROM_EMAIL_OCTOPUS"
}
```


### g_auth.json
download the service account JSON from google, make sure to enable the appropriate scopes, should look something like:
```json
{
  "type": "service_account",
  "project_id": "<project_id>",
  "private_key_id": "<privkey_id>",
  "private_key": "<privkey>",
  "client_email": "<client_email>",
  "client_id": "<client_id>",
  "auth_uri": "https://accounts.google.com/o/oauth2/auth",
  "token_uri": "https://oauth2.googleapis.com/token",
  "auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
  "client_x509_cert_url": "<cert_url>",
  "universe_domain": "googleapis.com"
}
```
### bb_auth.json
Same from the older blackbaud auth JSON, looks roughly like:
```json
{
    "other": {
        "api_subscription_key": "<api_subscription_key>",
        "test_api_endpoint": "<test_api_endpoint>",
        "redirect_uri": "http://localhost:13631/callback"
    },
    "tokens": {
        "access_token": "<access_token>",
        "refresh_token": "<refresh_token>"
    },
    "sky_app_information": {
        "app_id": "<app_id>",
        "app_secret": "<app_secret>"
    }
}
```

in order to generate octo_auth.json and bb_auth.json there was a helper created, just run:
```bash
  bbextract auth
```
and walk through the steps of creating the authfiles, you will need to sign in on a browser for the blackbaud 0auth, once ran it will spit out the required auth files for email octopus and blackbaud


### Configuration:

#### config.json
```json
{
  "transcript_list_ids":["<list_id>"],
  "transcript_comments_id": "<list_id>",
  "parents_list_id": "<list_id>",
  "enrollment_list_ids": {
    "departed": "<list_id>",
    "enrolled": "<list_id>"
  },
  "attendance": {
    "level_ids": ["<list_id>", "<list_id>", "<list_id>"]
  },
  "postgres": {
    "database":"school_db",
    "user":"postgres",
    "password":"postgres",
    "address":"0.0.0.0",
    "port":"5432"
  },
  "google": {
    "ou_students_path": "/Students",
    "ou_student_fmt": "/Class of ",
    "admin_email": "techmgr@bush.edu"
  }
}

```
#### mailconfig.json

JSON containing the configuration for mailing lists in email octopus, fields include:
* name: this is so that you can easily identify *which* list we are operating on
* id: list ID in email octopus
* grades: list of student grades for the parent
Example:
```json
[
  {"name": "Upper School", "id":"7dadb030-8fb5-11ee-9a34-59d0c2d4a70b", "grades":[12,11,10,9]}
  {"name": "Lower School", "id":"7dadb030-8fb5-11ee-9a34-59d0c2d4a70b", "grades":[0,1,2,3,4,5]}
  {"name": "Middle School", "id":"7dadb030-8fb5-11ee-9a34-59d0c2d4a70b", "grades":[6,7,8]}
]
```


### File structure:
after all is said and done, the file structure should look something like:
```
bbextract/
├── g_auth.json          # Authentication file
├── octo_auth.json       # Authentication file
├── bb_auth.json         # Authentication file
├── flow.bat             # Batch script for running Prefect flow with logging
├── server.bat           # Run Prefect server
├── flow.py              # Prefect flow definition
├── mailconfig.json      # Mail configuration
├── config.json          # General configuration
```


## Database

the schema is included with schema.sql, go ahead and run the schema, and give the proper database credentials in the config. (Look up how to create and manage databases with PGAdmin)
