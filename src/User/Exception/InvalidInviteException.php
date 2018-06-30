<?php

namespace App\User\Exception;

class InvalidInviteException extends \InvalidArgumentException
{
    protected $message = 'Invalid invite';
}