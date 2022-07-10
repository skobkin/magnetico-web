<?php
declare(strict_types=1);

namespace App\User\Exception;

class InvalidInviteException extends \InvalidArgumentException
{
    protected $message = 'Invalid invite';
}
